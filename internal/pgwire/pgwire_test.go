package pgwire

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

// stubHandler answers SELECTs with a fixed two-column result (including a NULL)
// and errors on anything else — enough to exercise the protocol paths.
func stubHandler(sql string) (Result, error) {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sql)), "SELECT") {
		return Result{
			Columns: []string{"a", "b"},
			Rows:    [][]any{{"x", nil}, {float64(42), "y"}},
		}, nil
	}
	return Result{}, fmt.Errorf("only SELECT is supported")
}

func startServerH(t *testing.T, auth func(string) bool, h Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = Serve(ln, auth, h) }()
	return ln.Addr().String()
}

func startServer(t *testing.T, auth func(string) bool) string {
	return startServerH(t, auth, stubHandler)
}

// recHandler records every SQL string it receives so tests can assert on the
// statement that reached the backend after parameter substitution.
type recHandler struct {
	mu   sync.Mutex
	sqls []string
}

func (rh *recHandler) handle(sql string) (Result, error) {
	rh.mu.Lock()
	rh.sqls = append(rh.sqls, sql)
	rh.mu.Unlock()
	return stubHandler(sql)
}

func (rh *recHandler) last() string {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	if len(rh.sqls) == 0 {
		return ""
	}
	return rh.sqls[len(rh.sqls)-1]
}

// --- minimal frontend client ---

func sendStartup(conn net.Conn) {
	body := append(be32(protoV3), []byte("user\x00centauri\x00\x00")...)
	pkt := append(be32(len(body)+4), body...)
	_, _ = conn.Write(pkt)
}

func sendFE(conn net.Conn, typ byte, body []byte) {
	pkt := []byte{typ}
	pkt = append(pkt, be32(len(body)+4)...)
	pkt = append(pkt, body...)
	_, _ = conn.Write(pkt)
}

func recv(t *testing.T, r *bufio.Reader) (byte, []byte) {
	t.Helper()
	typ, body, err := readMessage(r)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	return typ, body
}

func drainToReady(t *testing.T, r *bufio.Reader) {
	t.Helper()
	for {
		typ, _ := recv(t, r)
		if typ == 'Z' {
			return
		}
	}
}

func parseRowDesc(body []byte) []string {
	n := int(binary.BigEndian.Uint16(body[:2]))
	cols := make([]string, 0, n)
	p := 2
	for i := 0; i < n; i++ {
		end := p
		for body[end] != 0 {
			end++
		}
		cols = append(cols, string(body[p:end]))
		p = end + 1 + 18 // skip NUL + 18 fixed bytes
	}
	return cols
}

func parseDataRow(body []byte) []*string {
	n := int(binary.BigEndian.Uint16(body[:2]))
	out := make([]*string, 0, n)
	p := 2
	for i := 0; i < n; i++ {
		l := int32(binary.BigEndian.Uint32(body[p : p+4]))
		p += 4
		if l < 0 {
			out = append(out, nil)
			continue
		}
		s := string(body[p : p+int(l)])
		out = append(out, &s)
		p += int(l)
	}
	return out
}

func TestSimpleQuery(t *testing.T) {
	addr := startServer(t, nil)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)

	sendStartup(conn)
	if typ, body := recv(t, r); typ != 'R' || binary.BigEndian.Uint32(body) != 0 {
		t.Fatalf("expected AuthenticationOk, got %q %v", typ, body)
	}
	drainToReady(t, r)

	sendFE(conn, 'Q', append([]byte("SELECT a, b FROM facts"), 0))

	typ, body := recv(t, r)
	if typ != 'T' {
		t.Fatalf("expected RowDescription, got %q", typ)
	}
	if cols := parseRowDesc(body); len(cols) != 2 || cols[0] != "a" || cols[1] != "b" {
		t.Fatalf("columns = %v", cols)
	}

	typ, body = recv(t, r)
	if typ != 'D' {
		t.Fatalf("expected DataRow, got %q", typ)
	}
	row := parseDataRow(body)
	if len(row) != 2 || row[0] == nil || *row[0] != "x" || row[1] != nil {
		t.Fatalf("row1 = %v (want [x, NULL])", row)
	}

	typ, body = recv(t, r)
	row = parseDataRow(body)
	if typ != 'D' || row[0] == nil || *row[0] != "42" || row[1] == nil || *row[1] != "y" {
		t.Fatalf("row2 = %v (want [42, y])", row)
	}

	if typ, body := recv(t, r); typ != 'C' || cstring(body) != "SELECT 2" {
		t.Fatalf("CommandComplete = %q %q", typ, cstring(body))
	}
	if typ, _ := recv(t, r); typ != 'Z' {
		t.Fatalf("expected ReadyForQuery, got %q", typ)
	}
}

func TestAuthAndError(t *testing.T) {
	addr := startServer(t, VerifyAny("secret"))

	// Wrong password is rejected.
	bad, _ := net.Dial("tcp", addr)
	defer bad.Close()
	rb := bufio.NewReader(bad)
	sendStartup(bad)
	if typ, body := recv(t, rb); typ != 'R' || binary.BigEndian.Uint32(body) != 3 {
		t.Fatalf("expected cleartext-password request, got %q %v", typ, body)
	}
	sendFE(bad, 'p', append([]byte("wrong"), 0))
	if typ, _ := recv(t, rb); typ != 'E' {
		t.Fatalf("wrong password should yield ErrorResponse, got %q", typ)
	}

	// Correct password authenticates; a handler error is reported but the
	// connection stays usable (ReadyForQuery follows).
	good, _ := net.Dial("tcp", addr)
	defer good.Close()
	rg := bufio.NewReader(good)
	sendStartup(good)
	if typ, body := recv(t, rg); typ != 'R' || binary.BigEndian.Uint32(body) != 3 {
		t.Fatalf("expected cleartext-password request, got %q", typ)
	}
	sendFE(good, 'p', append([]byte("secret"), 0))
	if typ, body := recv(t, rg); typ != 'R' || binary.BigEndian.Uint32(body) != 0 {
		t.Fatalf("expected AuthenticationOk after correct password, got %q %v", typ, body)
	}
	drainToReady(t, rg)

	sendFE(good, 'Q', append([]byte("UPDATE facts SET x=1"), 0))
	if typ, _ := recv(t, rg); typ != 'E' {
		t.Fatalf("write should yield ErrorResponse, got %q", typ)
	}
	if typ, _ := recv(t, rg); typ != 'Z' {
		t.Fatalf("expected ReadyForQuery after error, got %q", typ)
	}
}

// --- extended (prepared-statement) protocol message builders ---

func feMsg(typ byte, body []byte) []byte {
	out := []byte{typ}
	out = append(out, be32(len(body)+4)...)
	return append(out, body...)
}

func feParse(name, sql string, oids []int32) []byte {
	var b []byte
	b = append(append(b, name...), 0)
	b = append(append(b, sql...), 0)
	b = append(b, be16(len(oids))...)
	for _, o := range oids {
		b = append(b, be32(int(o))...)
	}
	return feMsg('P', b)
}

func feBind(portal, stmt string, fmts []int16, params [][]byte) []byte {
	var b []byte
	b = append(append(b, portal...), 0)
	b = append(append(b, stmt...), 0)
	b = append(b, be16(len(fmts))...)
	for _, f := range fmts {
		b = append(b, be16(int(f))...)
	}
	b = append(b, be16(len(params))...)
	for _, pv := range params {
		if pv == nil {
			b = append(b, be32(-1)...)
			continue
		}
		b = append(b, be32(len(pv))...)
		b = append(b, pv...)
	}
	b = append(b, be16(0)...) // result format codes: none (text)
	return feMsg('B', b)
}

func feDescribe(kind byte, name string) []byte {
	b := append([]byte{kind}, name...)
	b = append(b, 0)
	return feMsg('D', b)
}

func feExecute(portal string, max int32) []byte {
	b := append([]byte(portal), 0)
	b = append(b, be32(int(max))...)
	return feMsg('E', b)
}

func connectExtended(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	sendStartup(conn)
	if typ, body := recv(t, r); typ != 'R' || binary.BigEndian.Uint32(body) != 0 {
		t.Fatalf("expected AuthenticationOk, got %q", typ)
	}
	drainToReady(t, r)
	return conn, r
}

// TestExtendedProtocol drives the full Parse/Bind/Describe(portal)/Execute/Sync
// flow that JDBC and psycopg use by default, with a text parameter, and asserts
// both the wire responses and the substituted SQL the backend received.
func TestExtendedProtocol(t *testing.T) {
	rh := &recHandler{}
	addr := startServerH(t, nil, rh.handle)
	conn, r := connectExtended(t, addr)
	defer conn.Close()

	_, _ = conn.Write(feParse("", "SELECT a, b FROM facts WHERE c = $1", nil))
	_, _ = conn.Write(feBind("", "", nil, [][]byte{[]byte("x")}))
	_, _ = conn.Write(feDescribe('P', ""))
	_, _ = conn.Write(feExecute("", 0))
	_, _ = conn.Write(feMsg('S', nil)) // Sync

	if typ, _ := recv(t, r); typ != '1' {
		t.Fatalf("expected ParseComplete, got %q", typ)
	}
	if typ, _ := recv(t, r); typ != '2' {
		t.Fatalf("expected BindComplete, got %q", typ)
	}
	if typ, body := recv(t, r); typ != 'T' || len(parseRowDesc(body)) != 2 {
		t.Fatalf("expected RowDescription(2), got %q", typ)
	}
	for i := 0; i < 2; i++ {
		if typ, _ := recv(t, r); typ != 'D' {
			t.Fatalf("expected DataRow, got %q", typ)
		}
	}
	if typ, body := recv(t, r); typ != 'C' || cstring(body) != "SELECT 2" {
		t.Fatalf("expected CommandComplete, got %q %q", typ, cstring(body))
	}
	if typ, _ := recv(t, r); typ != 'Z' {
		t.Fatalf("expected ReadyForQuery, got %q", typ)
	}
	if got := rh.last(); got != "SELECT a, b FROM facts WHERE c = 'x'" {
		t.Fatalf("substituted SQL = %q (text param should be quoted)", got)
	}
}

// TestExtendedDescribeStatement covers Describe(statement) — the prepare-time
// metadata round trip JDBC uses — and a numeric parameter (typed int4), which
// must be substituted unquoted.
func TestExtendedDescribeStatement(t *testing.T) {
	rh := &recHandler{}
	addr := startServerH(t, nil, rh.handle)
	conn, r := connectExtended(t, addr)
	defer conn.Close()

	_, _ = conn.Write(feParse("st1", "SELECT a, b FROM facts WHERE n = $1", []int32{23})) // int4
	_, _ = conn.Write(feDescribe('S', "st1"))
	_, _ = conn.Write(feMsg('S', nil))

	if typ, _ := recv(t, r); typ != '1' {
		t.Fatalf("expected ParseComplete, got %q", typ)
	}
	typ, body := recv(t, r)
	if typ != 't' {
		t.Fatalf("expected ParameterDescription, got %q", typ)
	}
	if n := int(binary.BigEndian.Uint16(body[:2])); n != 1 {
		t.Fatalf("param count = %d, want 1", n)
	}
	if oid := binary.BigEndian.Uint32(body[2:6]); oid != 23 {
		t.Fatalf("param oid = %d, want 23", oid)
	}
	if typ, body := recv(t, r); typ != 'T' || len(parseRowDesc(body)) != 2 {
		t.Fatalf("expected RowDescription(2), got %q", typ)
	}
	if typ, _ := recv(t, r); typ != 'Z' {
		t.Fatalf("expected ReadyForQuery, got %q", typ)
	}
	if got := rh.last(); got != "SELECT a, b FROM facts WHERE n = 0" {
		t.Fatalf("describe dummy SQL = %q (placeholder should become 0)", got)
	}

	// Now bind the numeric parameter and execute; int4 → unquoted literal.
	_, _ = conn.Write(feBind("p1", "st1", nil, [][]byte{[]byte("42")}))
	_, _ = conn.Write(feExecute("p1", 0))
	_, _ = conn.Write(feMsg('S', nil))
	for _, want := range []byte{'2', 'D', 'D', 'C', 'Z'} {
		if typ, _ := recv(t, r); typ != want {
			t.Fatalf("expected %q, got %q", want, typ)
		}
	}
	if got := rh.last(); got != "SELECT a, b FROM facts WHERE n = 42" {
		t.Fatalf("substituted SQL = %q (numeric param should be unquoted)", got)
	}
}
