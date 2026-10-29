/* Centauri site assistant — a floating chat that knows the whole product.
 *
 * Zero tokens, zero API keys: it runs entirely in the browser with the same
 * BM25 ranking Centauri's own SEARCH uses, over two sources it fetches from
 * this site:
 *   kb.json          — the assistant knowledge base (also embedded in the binary)
 *   chat-index.json  — every section of documentation.html (id, title, keywords)
 * plus whatever headings/cards are on the current page. Unanswered questions
 * are kept in localStorage and can be filed as a GitHub issue in one click.
 */
(function () {
  if (window.__centauriChat) return; window.__centauriChat = true;
  var BASE = (function () {
    var s = document.currentScript && document.currentScript.src;
    if (!s) return '';
    return s.replace(/[^\/]*$/, '');
  })();
  var DOCS = BASE + 'documentation.html';

  /* ---------- styles ---------- */
  var css = '' +
    '.cchat-btn{position:fixed;right:20px;bottom:20px;z-index:9000;border:0;cursor:pointer;background:linear-gradient(135deg,#00add8,#2f6fb0);color:#fff;font:700 15px system-ui,"Segoe UI",Roboto,Helvetica,sans-serif;border-radius:30px;padding:13px 20px;box-shadow:0 10px 30px rgba(20,40,80,.25);display:flex;align-items:center;gap:8px}' +
    '.cchat-btn:hover{filter:brightness(1.07)}' +
    '.cchat{position:fixed;right:20px;bottom:84px;z-index:9001;width:400px;max-width:calc(100vw - 24px);height:600px;max-height:calc(100vh - 110px);background:#fff;border:1px solid #dbe5f0;border-radius:16px;box-shadow:0 20px 60px rgba(20,40,80,.28);display:none;flex-direction:column;overflow:hidden;font:15px/1.55 system-ui,"Segoe UI",Roboto,Helvetica,sans-serif;color:#16243a}' +
    '.cchat.open{display:flex}' +
    '.cchat-head{background:#16212e;color:#fff;padding:12px 16px;display:flex;align-items:center;gap:9px;font-weight:700}' +
    '.cchat-head small{margin-left:auto;font-family:ui-monospace,Menlo,Consolas,monospace;font-size:10.5px;color:#8fb6d8;font-weight:500}' +
    '.cchat-x{background:none;border:0;color:#c6d4e6;cursor:pointer;font-size:18px;padding:0 0 0 8px}' +
    '.cchat-log{flex:1;overflow:auto;padding:14px;background:#f4f8fc}' +
    '.cchat-m{max-width:92%;margin:0 0 10px;padding:10px 13px;border-radius:12px;font-size:14.5px}' +
    '.cchat-m.u{background:#eaf1f8;margin-left:auto;border-bottom-right-radius:4px}' +
    '.cchat-m.b{background:#fff;border:1px solid #dbe5f0;border-bottom-left-radius:4px}' +
    '.cchat-m code{font-family:ui-monospace,Menlo,Consolas,monospace;font-size:.88em;background:#f5f8fc;border:1px solid #dbe5f0;border-radius:4px;padding:1px 5px}' +
    '.cchat-m a{color:#2f6fb0;text-decoration:none}.cchat-m a:hover{text-decoration:underline}' +
    '.cchat-links{margin-top:8px;font-size:13px;color:#54627a}' +
    '.cchat-links a{display:block;padding:3px 0}' +
    '.cchat-chips{display:flex;gap:6px;flex-wrap:wrap;margin-top:8px}' +
    '.cchat-chip{font-size:12.5px;color:#2f6fb0;background:#eaf1f8;border:1px solid #dbe5f0;border-radius:16px;padding:4px 10px;cursor:pointer}' +
    '.cchat-chip:hover{background:#dfeaf6}' +
    '.cchat-in{display:flex;gap:8px;padding:10px;border-top:1px solid #dbe5f0;background:#fff}' +
    '.cchat-in input{flex:1;border:1px solid #dbe5f0;border-radius:9px;padding:11px 13px;font:14.5px system-ui;color:#16243a;min-width:0}' +
    '.cchat-in input:focus{outline:none;border-color:#0594bb}' +
    '.cchat-in button{border:0;background:#00add8;color:#06222b;font-weight:700;border-radius:9px;padding:0 16px;cursor:pointer}' +
    '.cchat-in button:hover{background:#0594bb;color:#fff}' +
    '@media(max-width:480px){.cchat{right:12px;left:12px;width:auto;bottom:76px}.cchat-btn{right:12px;bottom:12px}}';
  var st = document.createElement('style'); st.textContent = css; document.head.appendChild(st);

  /* ---------- DOM ---------- */
  var btn = document.createElement('button'); btn.className = 'cchat-btn'; btn.type = 'button'; btn.innerHTML = '✦ Ask Centauri';
  var box = document.createElement('div'); box.className = 'cchat'; box.setAttribute('role', 'dialog'); box.setAttribute('aria-label', 'Centauri assistant');
  box.innerHTML =
    '<div class="cchat-head">🤖 Centauri Assistant <small>BM25 · 0 tokens · in your browser</small><button class="cchat-x" aria-label="Close">✕</button></div>' +
    '<div class="cchat-log"></div>' +
    '<form class="cchat-in"><input type="text" placeholder="Ask about any Centauri feature…" autocomplete="off" aria-label="Your question"><button type="submit">Ask</button></form>';
  document.body.appendChild(btn); document.body.appendChild(box);
  var log = box.querySelector('.cchat-log'), form = box.querySelector('form'), inp = box.querySelector('input');
  function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }
  function md(s) { return String(s).replace(/`([^`]+)`/g, '<code>$1</code>').replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>').replace(/\n/g, '<br>'); }
  function add(kind, html) { var d = document.createElement('div'); d.className = 'cchat-m ' + kind; d.innerHTML = html; log.appendChild(d); log.scrollTop = log.scrollHeight; return d; }
  function chips(container, qs) {
    var w = document.createElement('div'); w.className = 'cchat-chips';
    qs.forEach(function (q) { var c = document.createElement('span'); c.className = 'cchat-chip'; c.textContent = q; c.onclick = function () { ask(q); }; w.appendChild(c); });
    container.appendChild(w); log.scrollTop = log.scrollHeight;
  }

  /* ---------- knowledge ---------- */
  var KB = [];          // {q, t, a, link?, w?}  w = source weight: kb.json 1, doc sections .95, product tabs .75, page cards .6
  var docsIdx = [], VOCAB = [];
  function tok(s) { var o = [], b = ''; s = String(s).toLowerCase(); for (var i = 0; i < s.length; i++) { var ch = s[i]; if (/[a-z0-9]/.test(ch)) b += ch; else if (b) { o.push(b); b = ''; } } if (b) o.push(b); return o; }
  function uniq(a) { var s = {}, o = []; a.forEach(function (x) { if (!s[x]) { s[x] = 1; o.push(x); } }); return o; }
  function rebuild() { docsIdx = KB.map(function (e) { return tok(e.q + ' ' + e.t); }); var s = {}; docsIdx.forEach(function (d) { d.forEach(function (w) { s[w] = 1; }); }); VOCAB = Object.keys(s); }
  function within1(a, b) { if (a === b) return true; var la = a.length, lb = b.length; if (Math.abs(la - lb) > 1) return false; var i = 0, j = 0, diff = 0; while (i < la && j < lb) { if (a[i] === b[j]) { i++; j++; } else { if (++diff > 1) return false; if (la > lb) i++; else if (lb > la) j++; else { i++; j++; } } } return diff + (la - i) + (lb - j) <= 1; }
  var SYN = { db: 'database', dbs: 'databases', pg: 'postgres', postgre: 'postgresql', postgres: 'postgresql', vektor: 'vector', k8s: 'kubernetes', rdbms: 'relational', ollama: 'ollama', llm: 'llm', gpt: 'openai', chatgpt: 'openai', kong: 'kong', s3: 's3', bitemporal: 'bi-temporal', timetravel: 'time', licence: 'license', pricing: 'price', cost: 'price', vision: 'vision', pdf: 'pdf', pdfs: 'pdf', jdbc: 'jdbc', bi: 'bi', ha: 'ha', mcp: 'mcp' };
  function correct(t) { if (SYN[t]) return SYN[t]; if (t.length < 4 || VOCAB.indexOf(t) >= 0) return t; for (var i = 0; i < VOCAB.length; i++) { var w = VOCAB[i]; if (Math.abs(w.length - t.length) <= 1 && within1(t, w)) return w; } return t; }
  var STOP = {}; 'the is it its a an of to do does did can could would should how what why where when which who with for and or but my me i you your our this that these those are was were be been on in at as by from will not no yes about tell show explain please'.split(' ').forEach(function (w) { STOP[w] = 1; });
  function rankBM25(qterms) {
    var k1 = 1.5, b = 0.75, n = docsIdx.length, qset = {}, df = {}, tfs = [], total = 0; qterms.forEach(function (t) { qset[t] = 1; });
    docsIdx.forEach(function (d) { total += d.length; var tf = {}; d.forEach(function (w) { if (qset[w]) tf[w] = (tf[w] || 0) + 1; }); for (var w in tf) df[w] = (df[w] || 0) + 1; tfs.push(tf); });
    var avg = total / n || 1, idf = {}; for (var t in qset) idf[t] = Math.log(1 + (n - (df[t] || 0) + 0.5) / ((df[t] || 0) + 0.5));
    var out = [];
    for (var i = 0; i < n; i++) { var tf = tfs[i], keys = Object.keys(tf); if (!keys.length) continue; var dl = docsIdx[i].length, s = 0; keys.forEach(function (t) { var f = tf[t]; s += idf[t] * (f * (k1 + 1)) / (f + k1 * (1 - b + b * dl / avg)); }); out.push({ i: i, s: s * (KB[i].w || 1) }); }
    out.sort(function (a, b) { return b.s - a.s; }); return out;
  }

  // Overview answers for the broad questions people actually open a chatbot with.
  var OVERVIEW = {
    t: 'what can you do features capabilities everything overview products what is in the box help menu start',
    a: 'Centauri is one zero-dependency binary with six products inside. Ask me about any of them, or jump straight to the docs:' +
       '<div class="cchat-links">' +
       '<a href="' + DOCS + '#product-engine">🗄 Centauri Database — bi-temporal, causal, tamper-evident engine; CeQL, SQL, Postgres wire, hybrid search</a>' +
       '<a href="' + DOCS + '#product-studio">🖥 Studio, Console &amp; App — the embedded workspaces</a>' +
       '<a href="' + DOCS + '#product-ai">🧠 Local AI &amp; Vision — Gemma 3 / Qwen3 / GLM-4.7-Flash via Ollama; ASK with citations; vision ingest</a>' +
       '<a href="' + DOCS + '#product-gateway">🛡 Centauri Gateway — API &amp; LLM gateway with routes, keys and audit as facts (vs Kong, Apigee, AWS, Tyk)</a>' +
       '<a href="' + DOCS + '#product-agents">🤖 MCP server, Python/Go/JS SDKs, Airbyte connectors</a>' +
       '<a href="' + DOCS + '#product-deploy">☁️ Deploy &amp; scale — HA, sharding, replication, S3 cold tier, Kubernetes, Render</a>' +
       '</div>'
  };

  function sectionLinks(terms, n) {
    var hits = [];
    for (var i = 0; i < KB.length; i++) if (KB[i].link) {
      var d = docsIdx[i], c = 0; terms.forEach(function (t) { if (d.indexOf(t) >= 0) c++; });
      if (c) hits.push({ i: i, c: c });
    }
    hits.sort(function (a, b) { return b.c - a.c; });
    return hits.slice(0, n).map(function (h) { return KB[h.i]; });
  }
  function pin(r, q) { var i = -1; KB.forEach(function (e, k) { if (e.q === q) i = k; }); if (i < 0) return r; return [{ i: i, s: 99 }].concat(r.filter(function (x) { return x.i !== i; })); }
  function logGap(q) { try { var k = 'centauri_unanswered', l = JSON.parse(localStorage.getItem(k) || '[]'); l.push({ q: q, at: Date.now() }); localStorage.setItem(k, JSON.stringify(l.slice(-200))); } catch (e) { } }

  function ask(text) {
    text = String(text || '').trim(); if (!text) return;
    inp.value = '';
    add('u', esc(text));
    var terms = uniq(tok(text).map(correct)).filter(function (w) { return w.length > 1 && !STOP[w]; });
    var r = terms.length ? rankBM25(terms) : [];
    var low = text.toLowerCase();
    if (/\b(not do|doesn'?t do|limitations?|limits|drawbacks|weakness|can'?t it do|cannot)\b/.test(low)) r = pin(r, 'What does Centauri NOT do limitations');
    else if (/^(what (can|does) (centauri|it|you) do|what is (centauri|this|in the box)|features|capabilities|everything|help|menu|overview)/.test(low) || /\b(all (the )?(features|capabilities)|everything it (can )?do)\b/.test(low)) r = pin(r, 'What can Centauri do?');
    var html = '', related = [];
    if (r.length && r[0].s >= 0.6) {
      var best = KB[r[0].i];
      html += best.link ? ('<b>' + esc(best.q) + '</b><br>' + md(best.a) + '<div class="cchat-links"><a href="' + best.link + '">Read the section →</a></div>') : md(best.a);
      var links = sectionLinks(terms, 3).filter(function (e) { return e !== best; });
      if (links.length) html += '<div class="cchat-links">In the docs: ' + links.map(function (e) { return '<a href="' + e.link + '">' + esc(e.q) + ' →</a>'; }).join('') + '</div>';
      r.slice(1, 6).forEach(function (x) { if (x.s >= 0.8 && !KB[x.i].link && related.length < 3) related.push(KB[x.i].q); });
    } else {
      logGap(text);
      var issue = 'https://github.com/aniljacobv-lab/centauri/issues/new?title=' + encodeURIComponent('FAQ: ' + text) + '&body=' + encodeURIComponent('Question the site assistant could not answer — suggest it be added to the knowledge base:\n\n> ' + text + '\n');
      html += "I haven't learned that one yet — I've <b>logged the question</b> so Centauri can learn it. You can <a href='" + issue + "' target='_blank' rel='noopener'>teach it the answer →</a>, or browse the <a href='" + DOCS + "'>documentation</a>.";
      related = ['What can Centauri do?', 'How do I install it?', 'Is it free?', 'How does the gateway compare to Kong?'];
    }
    var m = add('b', html);
    if (related.length) chips(m, related);
    try { sessionStorage.setItem('centauri_chat_open', '1'); } catch (e) { }
  }

  /* ---------- page content: index headings/cards on the current page too ---------- */
  try {
    var seen = {};
    document.querySelectorAll('article.s[id], .feat, .doc').forEach(function (el) {
      var h = el.querySelector('h3, b'), p = el.querySelector('p, span'); if (!h) return;
      var q = h.innerText.replace(/#\s*$/, '').trim(), a = p ? p.innerText.trim() : '';
      if (!q || seen[q.toLowerCase()]) return; seen[q.toLowerCase()] = 1;
      var link = el.id ? ('#' + el.id) : (el.tagName === 'A' ? el.getAttribute('href') : null);
      KB.push({ q: q, t: q + ' ' + (el.getAttribute('data-k') || ''), a: esc(a).slice(0, 400), link: link, w: el.id ? 0.95 : 0.6 });
    });
  } catch (e) { }
  KB.push({ q: 'What can Centauri do?', t: OVERVIEW.t, a: OVERVIEW.a });
  rebuild();

  var loaded = 0;
  function fetched() { if (++loaded === 2) rebuild(); }
  fetch(BASE + 'kb.json').then(function (r) { return r.ok ? r.json() : []; }).then(function (arr) {
    if (Array.isArray(arr)) { var have = {}; KB.forEach(function (e) { have[(e.q || '').toLowerCase()] = 1; });
      arr.forEach(function (e) { var k = (e.q || '').toLowerCase(); if (e && e.q && e.a && !have[k]) { have[k] = 1; KB.push({ q: e.q, t: e.t || '', a: e.a }); } }); }
    fetched();
  }).catch(fetched);
  fetch(BASE + 'chat-index.json').then(function (r) { return r.ok ? r.json() : []; }).then(function (arr) {
    if (Array.isArray(arr)) { var onDocs = /documentation\.html$/.test(location.pathname) || /\/documentation\.html/.test(location.href);
      arr.forEach(function (e) { if (!e || !e.id) return;
        var link = (onDocs ? '#' : DOCS + '#') + e.id;
        var exists = false; KB.forEach(function (k) { if (k.link === '#' + e.id) { exists = true; k.t += ' ' + e.k; } });
        if (!exists) KB.push({ q: e.title, t: e.title + ' ' + e.track + ' ' + e.k, a: e.x || ('Section of the documentation: ' + e.track + '.'), link: link, w: e.x ? 0.95 : 0.75 });
      }); }
    fetched();
  }).catch(fetched);

  /* ---------- wiring ---------- */
  var greeted = false;
  function open() {
    box.classList.add('open'); btn.style.display = 'none'; inp.focus();
    if (!greeted) {
      greeted = true;
      var m = add('b', 'Hi — I know the whole Centauri product: the database engine, CeQL and SQL, the local AI and vision stack, the Genesis architect, the MCP server and SDKs, the gateway, storage, HA and deployment. Ask me anything, or pick a question:');
      chips(m, ['What can Centauri do?', 'How do I install it?', 'Which local models does it use?', 'How does the gateway compare to Kong?', 'Can I connect psql or a BI tool?', 'What does Centauri NOT do?']);
    }
  }
  function close() { box.classList.remove('open'); btn.style.display = ''; try { sessionStorage.removeItem('centauri_chat_open'); } catch (e) { } }
  btn.onclick = open; box.querySelector('.cchat-x').onclick = close;
  form.addEventListener('submit', function (e) { e.preventDefault(); ask(inp.value); });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape' && box.classList.contains('open')) close(); });
  window.centauriAsk = function (q) { open(); ask(q); };
})();
