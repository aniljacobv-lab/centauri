// Package pgwire speaks the PostgreSQL frontend/backend wire protocol (v3) so
// off-the-shelf SQL clients — psql, JDBC/ODBC drivers, DBeaver, Tableau, Power
// BI — can connect to Centauri directly and run SELECTs. It is implemented with
// ONLY the Go standard library (net, encoding/binary), keeping the zero-Go-
// dependency invariant: this is a thin protocol adapter in front of the SQL→CeQL
// transpiler (internal/ceql.ParseSQL), not a new query engine.
//
// Scope: both query sub-protocols are implemented. The SIMPLE protocol ('Q')
// runs a statement in one round trip. The EXTENDED protocol (Parse / Bind /
// Describe / Execute / Sync / Close) backs prepared statements with parameters,
// which is what JDBC and psycopg use by default — so those drivers work without
// forcing simple query mode. Parameters (text or binary wire format) are
// rendered into SQL literals and substituted before the statement is transpiled
// to CeQL; column metadata for Describe is obtained by running the statement
// with placeholder values (the column shape does not depend on the values).
// Every column is returned as TEXT (OID 25); the data is read-only — writes must
// still use CeQL.
package pgwire

import (
	"bufio"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
)

// Postgres protocol constants.
const (
	protoV3       = 196608   // 0x00030000
	sslRequest    = 80877103 // magic code: client asks to start TLS
	gssEncRequest = 80877104 // magic code: client asks for GSSAPI encryption
	typeOIDText   = 25       // we render every column as text
)

// Result is one statement's output. Each cell in Rows is rendered to text by
// the protocol layer; a nil cell is SQL NULL. Tag is the CommandComplete tag
// (e.g. "SELECT 5"); when empty the server derives "SELECT <rowcount>".
type Result struct {
	Columns []string
	Rows    [][]any
	Tag     string
}

// Handler executes one SQL statement. A returned error becomes a Postgres
// ErrorResponse; the connection stays open for the next query.
type Handler func(sql string) (Result, error)

// Serve accepts connections on ln and handles each in its own goroutine until
// ln is closed. When auth is non-nil, clients must present a cleartext password
// for which auth returns true; when nil, connections are accepted without a
// password (use TLS / network controls in that case).
func Serve(ln net.Listener, auth func(password string) bool, h Handler) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go serveConn(c, auth, h)
	}
}

func serveConn(c net.Conn, auth func(string) bool, h Handler) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	if err := handshake(r, w, auth); err != nil {
		return
	}
	s := &session{
		w:       w,
		h:       h,
		stmts:   map[string]*prepStmt{},
		portals: map[string]*boundPortal{},
	}
	for {
		typ, body, err := readMessage(r)
		if err != nil {
			return
		}
		switch typ {
		case 'Q': // Simple Query
			s.errored = false
			handleQuery(w, h, cstring(body))
		case 'P': // Parse
			s.handleParse(body)
		case 'B': // Bind
			s.handleBind(body)
		case 'D': // Describe
			s.handleDescribe(body)
		case 'E': // Execute
			s.handleExecute(body)
		case 'C': // Close
			s.handleClose(body)
		case 'H': // Flush — force the buffered output out (handled by Flush below)
		case 'S': // Sync — end of an extended-protocol cycle
			s.errored = false
			writeReadyForQuery(w)
		case 'X': // Terminate
			return
		default:
			// Unknown message: ignore (be liberal in what we accept).
		}
		if err := w.Flush(); err != nil {
			return
		}
	}
}

// session holds the per-connection extended-protocol state: named (and unnamed,
// "") prepared statements and bound portals. When errored is set, extended
// messages are skipped until the next Sync — the protocol's error-recovery rule.
type session struct {
	w       *bufio.Writer
	h       Handler
	stmts   map[string]*prepStmt
	portals map[string]*boundPortal
	errored bool
}

type prepStmt struct {
	sql  string  // may contain $1.. placeholders
	oids []int32 // parameter type OIDs as declared at Parse (may be empty)
}

type boundPortal struct {
	sql      string // placeholders substituted with bound literals
	result   *Result
	execErr  error
	executed bool
}

func (s *session) fail(code, msg string) {
	writeError(s.w, code, msg)
	s.errored = true // skip the rest of the batch until Sync
}

// ensureExecuted runs the portal's SQL once and caches the result/error so a
// Describe(portal) followed by Execute does not run the query twice.
func (s *session) ensureExecuted(p *boundPortal) {
	if p.executed {
		return
	}
	res, err := s.h(p.sql)
	p.result, p.execErr, p.executed = &res, err, true
}

func (s *session) handleParse(body []byte) {
	if s.errored {
		return
	}
	c := &cursor{b: body}
	name := c.cstr()
	sql := c.cstr()
	n := c.u16()
	oids := make([]int32, 0, n)
	for i := 0; i < n; i++ {
		oids = append(oids, c.i32())
	}
	s.stmts[name] = &prepStmt{sql: sql, oids: oids}
	writeMsg(s.w, '1', nil) // ParseComplete
}

func (s *session) handleBind(body []byte) {
	if s.errored {
		return
	}
	c := &cursor{b: body}
	portalName := c.cstr()
	stmtName := c.cstr()
	st, ok := s.stmts[stmtName]
	if !ok {
		s.fail("26000", fmt.Sprintf("unknown prepared statement %q", stmtName))
		return
	}
	nfmt := c.u16()
	fmts := make([]int16, nfmt)
	for i := 0; i < nfmt; i++ {
		fmts[i] = int16(c.u16())
	}
	nval := c.u16()
	args := make([]string, nval)
	for i := 0; i < nval; i++ {
		l := c.i32()
		if l < 0 {
			args[i] = "NULL"
			continue
		}
		raw := c.take(int(l))
		format := int16(0)
		if nfmt == 1 {
			format = fmts[0]
		} else if i < nfmt {
			format = fmts[i]
		}
		var oid int32
		if i < len(st.oids) {
			oid = st.oids[i]
		}
		args[i] = renderLiteral(raw, format, oid)
	}
	// Remaining bytes are result-column format codes; we always reply in text,
	// so they are ignored.
	bound := substitute(st.sql, func(idx int) (string, bool) {
		if idx >= 1 && idx <= len(args) {
			return args[idx-1], true
		}
		return "", false
	})
	s.portals[portalName] = &boundPortal{sql: bound}
	writeMsg(s.w, '2', nil) // BindComplete
}

func (s *session) handleDescribe(body []byte) {
	if s.errored {
		return
	}
	c := &cursor{b: body}
	what := c.byte()
	name := c.cstr()
	switch what {
	case 'S': // describe a prepared statement (params + result columns)
		st, ok := s.stmts[name]
		if !ok {
			s.fail("26000", fmt.Sprintf("unknown prepared statement %q", name))
			return
		}
		writeParamDesc(s.w, st.oids)
		// Result columns don't depend on parameter values, so run the statement
		// with placeholder dummies just to learn the shape.
		dummy := substitute(st.sql, func(int) (string, bool) { return "0", true })
		res, err := s.h(dummy)
		if err != nil {
			s.fail("42601", err.Error())
			return
		}
		describeColumns(s.w, res.Columns)
	case 'P': // describe a bound portal
		portal, ok := s.portals[name]
		if !ok {
			s.fail("34000", fmt.Sprintf("unknown portal %q", name))
			return
		}
		s.ensureExecuted(portal)
		if portal.execErr != nil {
			s.fail("42601", portal.execErr.Error())
			return
		}
		describeColumns(s.w, portal.result.Columns)
	default:
		s.fail("08P01", "invalid Describe target")
	}
}

func (s *session) handleExecute(body []byte) {
	if s.errored {
		return
	}
	c := &cursor{b: body}
	name := c.cstr()
	_ = c.i32() // max rows: 0 = unlimited; we always return all
	portal, ok := s.portals[name]
	if !ok {
		s.fail("34000", fmt.Sprintf("unknown portal %q", name))
		return
	}
	s.ensureExecuted(portal)
	if portal.execErr != nil {
		s.fail("42601", portal.execErr.Error())
		return
	}
	for _, row := range portal.result.Rows {
		writeDataRow(s.w, row)
	}
	tag := portal.result.Tag
	if tag == "" {
		tag = fmt.Sprintf("SELECT %d", len(portal.result.Rows))
	}
	writeMsg(s.w, 'C', append([]byte(tag), 0)) // CommandComplete (no RowDescription in extended)
}

func (s *session) handleClose(body []byte) {
	if s.errored {
		return
	}
	c := &cursor{b: body}
	what := c.byte()
	name := c.cstr()
	switch what {
	case 'S':
		delete(s.stmts, name)
	case 'P':
		delete(s.portals, name)
	}
	writeMsg(s.w, '3', nil) // CloseComplete
}

// describeColumns emits RowDescription, or NoData when the statement yields no
// result columns (e.g. SET / BEGIN).
func describeColumns(w *bufio.Writer, cols []string) {
	if len(cols) == 0 {
		writeMsg(w, 'n', nil) // NoData
		return
	}
	writeRowDescription(w, cols)
}

// substitute rewrites $1, $2, … placeholders using resolve, skipping any that
// fall inside single-quoted string literals. resolve returns (literal, true) to
// replace, or (_, false) to leave the placeholder untouched.
func substitute(sql string, resolve func(idx int) (string, bool)) string {
	var b strings.Builder
	inStr := false
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		if ch == '\'' {
			inStr = !inStr
			b.WriteByte(ch)
			continue
		}
		if !inStr && ch == '$' && i+1 < len(sql) && sql[i+1] >= '1' && sql[i+1] <= '9' {
			j := i + 1
			for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
				j++
			}
			idx, _ := strconv.Atoi(sql[i+1 : j])
			if lit, ok := resolve(idx); ok {
				b.WriteString(lit)
				i = j - 1
				continue
			}
		}
		b.WriteByte(ch)
	}
	return b.String()
}

// renderLiteral turns a wire parameter (text or binary format) into a SQL
// literal: numeric/boolean types unquoted, everything else single-quoted.
func renderLiteral(raw []byte, format int16, oid int32) string {
	if format == 1 { // binary
		txt, numeric := decodeBinary(raw, oid)
		if numeric {
			return txt
		}
		return quoteLit(txt)
	}
	txt := string(raw)
	if isNumericOID(oid) || (oid == 0 && looksNumeric(txt)) {
		return txt
	}
	return quoteLit(txt)
}

func quoteLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func isNumericOID(oid int32) bool {
	switch oid {
	case 20, 21, 23, 26, 700, 701, 1700: // int8/int2/int4/oid/float4/float8/numeric
		return true
	}
	return false
}

func looksNumeric(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

// decodeBinary renders a binary-format parameter as text and reports whether it
// is a numeric/boolean literal (unquoted) value. Unknown OIDs fall back to the
// raw bytes as text.
func decodeBinary(raw []byte, oid int32) (string, bool) {
	switch oid {
	case 21:
		if len(raw) >= 2 {
			return strconv.FormatInt(int64(int16(binary.BigEndian.Uint16(raw))), 10), true
		}
	case 23:
		if len(raw) >= 4 {
			return strconv.FormatInt(int64(int32(binary.BigEndian.Uint32(raw))), 10), true
		}
	case 20:
		if len(raw) >= 8 {
			return strconv.FormatInt(int64(binary.BigEndian.Uint64(raw)), 10), true
		}
	case 700:
		if len(raw) >= 4 {
			return strconv.FormatFloat(float64(math.Float32frombits(binary.BigEndian.Uint32(raw))), 'f', -1, 32), true
		}
	case 701:
		if len(raw) >= 8 {
			return strconv.FormatFloat(math.Float64frombits(binary.BigEndian.Uint64(raw)), 'f', -1, 64), true
		}
	case 16:
		if len(raw) >= 1 {
			if raw[0] != 0 {
				return "true", false
			}
			return "false", false
		}
	}
	return string(raw), false
}

// cursor reads big-endian fields from a message body without panicking on a
// short/truncated message (it clamps to the end and yields zero values).
type cursor struct {
	b []byte
	p int
}

func (c *cursor) byte() byte {
	if c.p >= len(c.b) {
		return 0
	}
	v := c.b[c.p]
	c.p++
	return v
}

func (c *cursor) u16() int {
	if c.p+2 > len(c.b) {
		c.p = len(c.b)
		return 0
	}
	v := int(binary.BigEndian.Uint16(c.b[c.p : c.p+2]))
	c.p += 2
	return v
}

func (c *cursor) i32() int32 {
	if c.p+4 > len(c.b) {
		c.p = len(c.b)
		return 0
	}
	v := int32(binary.BigEndian.Uint32(c.b[c.p : c.p+4]))
	c.p += 4
	return v
}

func (c *cursor) take(n int) []byte {
	if n < 0 || c.p+n > len(c.b) {
		c.p = len(c.b)
		return nil
	}
	v := c.b[c.p : c.p+n]
	c.p += n
	return v
}

func (c *cursor) cstr() string {
	start := c.p
	for c.p < len(c.b) && c.b[c.p] != 0 {
		c.p++
	}
	s := string(c.b[start:c.p])
	if c.p < len(c.b) {
		c.p++ // skip NUL
	}
	return s
}

func writeParamDesc(w *bufio.Writer, oids []int32) {
	buf := be16(len(oids))
	for _, o := range oids {
		buf = append(buf, be32(int(o))...)
	}
	writeMsg(w, 't', buf) // ParameterDescription
}

// handshake negotiates SSL (declined), reads the v3 startup packet, performs
// optional cleartext-password auth, and emits the initial ParameterStatus /
// BackendKeyData / ReadyForQuery so the client reaches its prompt.
func handshake(r *bufio.Reader, w *bufio.Writer, auth func(string) bool) error {
	for {
		n, err := readInt32(r)
		if err != nil {
			return err
		}
		if n < 8 || n > 1<<20 {
			return fmt.Errorf("pgwire: bad startup length %d", n)
		}
		buf := make([]byte, n-4)
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		code := binary.BigEndian.Uint32(buf[:4])
		if code == sslRequest || code == gssEncRequest {
			// Decline encryption negotiation; the client may retry plaintext.
			if err := w.WriteByte('N'); err != nil {
				return err
			}
			if err := w.Flush(); err != nil {
				return err
			}
			continue
		}
		if code != protoV3 {
			return fmt.Errorf("pgwire: unsupported protocol code %d", code)
		}
		break // got a v3 startup packet (param key/value pairs follow; ignored)
	}

	if auth != nil {
		writeMsg(w, 'R', be32(3)) // AuthenticationCleartextPassword
		if err := w.Flush(); err != nil {
			return err
		}
		typ, body, err := readMessage(r)
		if err != nil {
			return err
		}
		if typ != 'p' || !auth(cstring(body)) {
			writeError(w, "28P01", "password authentication failed")
			_ = w.Flush()
			return fmt.Errorf("pgwire: auth failed")
		}
	}

	writeMsg(w, 'R', be32(0)) // AuthenticationOk
	writeParam(w, "server_version", "14.0 (Centauri)")
	writeParam(w, "client_encoding", "UTF8")
	writeParam(w, "DateStyle", "ISO, MDY")
	writeParam(w, "integer_datetimes", "on")
	writeMsg(w, 'K', append(be32(1), be32(1)...)) // BackendKeyData (pid, secret)
	writeReadyForQuery(w)
	return w.Flush()
}

func handleQuery(w *bufio.Writer, h Handler, sql string) {
	if strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sql), ";")) == "" {
		writeMsg(w, 'I', nil) // EmptyQueryResponse
		writeReadyForQuery(w)
		return
	}
	res, err := h(sql)
	if err != nil {
		writeError(w, "42601", err.Error())
		writeReadyForQuery(w)
		return
	}
	if len(res.Columns) > 0 {
		writeRowDescription(w, res.Columns)
	}
	for _, row := range res.Rows {
		writeDataRow(w, row)
	}
	tag := res.Tag
	if tag == "" {
		tag = fmt.Sprintf("SELECT %d", len(res.Rows))
	}
	writeMsg(w, 'C', append([]byte(tag), 0)) // CommandComplete
	writeReadyForQuery(w)
}

// ---- message writers ----

func writeRowDescription(w *bufio.Writer, cols []string) {
	buf := be16(len(cols))
	for _, c := range cols {
		buf = append(buf, []byte(c)...)
		buf = append(buf, 0)             // field name (null-terminated)
		buf = append(buf, be32(0)...)    // table OID
		buf = append(buf, be16(0)...)    // column attribute number
		buf = append(buf, be32(typeOIDText)...)
		buf = append(buf, be16(-1)...)   // type size (variable)
		buf = append(buf, be32(-1)...)   // type modifier
		buf = append(buf, be16(0)...)    // format code: 0 = text
	}
	writeMsg(w, 'T', buf)
}

func writeDataRow(w *bufio.Writer, row []any) {
	buf := be16(len(row))
	for _, v := range row {
		if v == nil {
			buf = append(buf, be32(-1)...) // NULL
			continue
		}
		s := render(v)
		buf = append(buf, be32(len(s))...)
		buf = append(buf, s...)
	}
	writeMsg(w, 'D', buf)
}

func writeError(w *bufio.Writer, code, msg string) {
	var buf []byte
	field := func(tag byte, s string) {
		buf = append(buf, tag)
		buf = append(buf, []byte(s)...)
		buf = append(buf, 0)
	}
	field('S', "ERROR")
	field('C', code)
	field('M', msg)
	buf = append(buf, 0) // final terminator
	writeMsg(w, 'E', buf)
}

func writeParam(w *bufio.Writer, k, v string) {
	body := append([]byte(k), 0)
	body = append(body, []byte(v)...)
	body = append(body, 0)
	writeMsg(w, 'S', body)
}

func writeReadyForQuery(w *bufio.Writer) { writeMsg(w, 'Z', []byte{'I'}) } // 'I' = idle

// writeMsg frames a backend message: type byte + int32 length (incl. itself).
func writeMsg(w *bufio.Writer, typ byte, body []byte) {
	_ = w.WriteByte(typ)
	_, _ = w.Write(be32(len(body) + 4))
	_, _ = w.Write(body)
}

// ---- message reader ----

func readMessage(r *bufio.Reader) (byte, []byte, error) {
	typ, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	n, err := readInt32(r)
	if err != nil {
		return 0, nil, err
	}
	if n < 4 || n > 1<<24 {
		return 0, nil, fmt.Errorf("pgwire: bad message length %d", n)
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return typ, body, nil
}

func readInt32(r *bufio.Reader) (int32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return int32(binary.BigEndian.Uint32(b[:])), nil
}

// ---- helpers ----

func be32(v int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(int32(v)))
	return b
}

func be16(v int) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, uint16(int16(v)))
	return b
}

// cstring returns the bytes up to the first NUL as a string.
func cstring(b []byte) string {
	if i := indexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// render encodes a cell value as Postgres text. Floats avoid scientific
// notation so price-like integers stored as JSON numbers read naturally.
func render(v any) []byte {
	switch t := v.(type) {
	case string:
		return []byte(t)
	case []byte:
		return t
	case float64:
		return []byte(strconv.FormatFloat(t, 'f', -1, 64))
	case float32:
		return []byte(strconv.FormatFloat(float64(t), 'f', -1, 64))
	case bool:
		if t {
			return []byte("t")
		}
		return []byte("f")
	default:
		return []byte(fmt.Sprint(v))
	}
}

// VerifyAny returns an auth func accepting any of the given non-empty passwords
// (constant-time), or nil when none are set (open access).
func VerifyAny(passwords ...string) func(string) bool {
	var allowed []string
	for _, p := range passwords {
		if p != "" {
			allowed = append(allowed, p)
		}
	}
	if len(allowed) == 0 {
		return nil
	}
	return func(got string) bool {
		ok := false
		for _, p := range allowed {
			if subtle.ConstantTimeCompare([]byte(got), []byte(p)) == 1 {
				ok = true
			}
		}
		return ok
	}
}
