(function () {
  'use strict';
  const $ = (s, root = document) => root.querySelector(s);
  const t = (s) => window.dorangI18n ? window.dorangI18n.t(s) : s;
  const node = (tag, text, cls) => { const e = document.createElement(tag); if (text !== undefined) e.textContent = text; if (cls) e.className = cls; return e; };
  const fmt = (v, digits = 0) => Number.isFinite(Number(v)) ? Number(v).toLocaleString(document.documentElement.lang, { maximumFractionDigits: digits }) : '—';
  const money = (v) => v == null ? '—' : '$' + fmt(v, 6);
  const set = (id, value) => { const e = document.getElementById(id); if (e) e.textContent = value; };
  const emptyRow = (body, count, text) => { const row = node('tr'); const td = node('td', t(text), 'empty-cell'); td.colSpan = count; row.append(td); body.append(row); };
  const badge = (status) => node('span', String(status), 'status-badge ' + (Number(status) >= 400 ? 'is-error' : 'is-ok'));

  // Parse the Prometheus text grammar's samples and escaped string labels.
  // Keep buckets and non-finite values: they remain inspectable in the explorer.
  function parseMetrics(raw) {
    const types = {}, help = {}, samples = [];
    for (const line of raw.split('\n')) {
      let m = line.match(/^# (TYPE|HELP) (\S+) (.*)$/);
      if (m) { (m[1] === 'TYPE' ? types : help)[m[2]] = m[3]; continue; }
      m = line.match(/^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{(?:[^"}]|"(?:\\.|[^"\\])*")*\})?\s+(\S+)/);
      if (!m) continue;
      const labels = {};
      for (const pair of (m[2] || '').matchAll(/([a-zA-Z_][a-zA-Z0-9_]*)="((?:\\.|[^"\\])*)"/g)) {
        try { labels[pair[1]] = JSON.parse('"' + pair[2] + '"'); } catch (_) { labels[pair[1]] = pair[2]; }
      }
      const name = m[1], family = types[name] ? name : name.replace(/_(bucket|sum|count)$/, '');
      samples.push({ name, family, labels, key: name + (m[2] || ''), value: Number(m[3]), raw: m[3], type: types[family] || 'untyped', help: help[family] || '' });
    }
    return samples;
  }
  window.dorangMetrics = { parse: parseMetrics };

  document.addEventListener('DOMContentLoaded', () => {
    document.querySelectorAll('[data-close-dialog]').forEach(b => b.addEventListener('click', () => b.closest('dialog').close()));
    const commands = $('#command-dialog'), opener = $('#command-open');
    if (commands && opener) {
      const renderCommands = () => {
        const out = $('#command-results'), q = $('#command-query').value.toLowerCase(); out.replaceChildren();
        document.querySelectorAll('.sidebar .nav-link').forEach(link => {
          if (link.textContent.toLowerCase().includes(q)) { const a = node('a', link.textContent.trim(), 'command-result'); a.href = link.href; out.append(a); }
        });
      };
      const open = () => { commands.showModal(); $('#command-query').value = ''; renderCommands(); $('#command-query').focus(); };
      opener.addEventListener('click', open); $('#command-query').addEventListener('input', renderCommands);
      document.addEventListener('keydown', e => { if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); if (commands.open) commands.close(); else open(); } });
      $('#command-query').addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); const a = $('#command-results a'); if (a) a.click(); } });
    }
    document.querySelectorAll('form[data-soft-kinds]').forEach(form=>{
      const kind=form.elements.subject_kind,soft=form.elements.soft_budget;
      const update=()=>{soft.disabled=!form.dataset.softKinds.split(' ').includes(kind.value);};
      kind.addEventListener('change',update);update();
    });
    // The native form remains the source of the action and CSRF token.
    document.querySelectorAll('form[data-confirm]').forEach(form => form.addEventListener('submit', e => {
      if (!window.confirm(t(form.dataset.confirm))) e.preventDefault();
    }));
    const root = $('[data-console]');
    if (!root) return;
    const descriptions = {
      overview: 'Traffic, token use and model health in one live workspace.',
      requests: 'Inspect completed requests as they arrive. Pause to keep a request in view.',
      history: 'Find durable request records by time, identity or trace.',
      metrics: 'Explore every published Prometheus series by category, name and labels.',
      credentials: 'Provider admission health and every quota window. Circuit state is available in Prometheus.',
      capacity: 'See where requests are waiting and which limits are saturated.',
      analytics: 'Compare requests, errors, tokens and cost across the fleet.',
      budgets: 'Control spending limits and see consumed, reserved and remaining amounts.',
      pricing: 'Preview the rule chain and token charges without sending an inference request.',
      catalog: 'Inspect model capabilities and the source of each configured value.',
      operations: 'Inspect connected subsystems, reload configuration and find API capabilities.'
    };
    const description = () => { const p = $('[data-page-description]'); if (p) p.textContent = t(descriptions[root.dataset.console] || ''); };
    description();
    document.addEventListener('dorang:language', description);
    if (!root.dataset.stream) return;
    let streamLabel='Connecting', streamClass='', stream, paused = false, latest = null, samples = [], previous = null, rates = new Map(), history = [], received = 0;
    let modelQuery = '', requestQuery = new URLSearchParams(location.search).get('model') || '', statusQuery = 'all', metricQuery = '', category = 'all', credentialQuery = '';
    const metricOpen = new Set();
    const sum = (name, test = () => true) => samples.filter(s => s.name === name && test(s)).reduce((n, s) => n + (Number.isFinite(s.value) ? s.value : 0), 0);
    const state = (label, cls) => { streamLabel=label;streamClass=cls||'';const e = $('#stream-state'); if (e) { e.textContent = t(label); e.className = 'stream-state ' + (cls || ''); } };
    function updateRate(data) {
      const now = Date.parse(data.at), sf = data.surface, id = sf && sf.NodeID;
      const values = new Map(samples.map(s => [s.key, s.value])); rates = new Map();
      if (previous && previous.id === id && now > previous.at) {
        const sec = (now - previous.at) / 1000;
        for (const s of samples) {
          const before = previous.values.get(s.key);
          if ((s.type === 'counter' || /_(count|sum|bucket)$/.test(s.name)) && Number.isFinite(before) && s.value >= before) rates.set(s.key, (s.value - before) / sec);
        }
        const requests = sum('dorang_requests_total'), errors = sum('dorang_requests_total', s => Number(s.labels.status) >= 400);
        if (requests >= previous.requests && errors >= previous.errors) history.push({ r: (requests - previous.requests) / sec, e: (errors - previous.errors) / sec }); else history = [];
        history = history.slice(-60);
      } else if (!previous || previous.id !== id || (sf && sf.UptimeSeconds < previous.uptime)) history = [];
      previous = { id, at: now, values, requests: sum('dorang_requests_total'), errors: sum('dorang_requests_total', s => Number(s.labels.status) >= 400), uptime: sf && sf.UptimeSeconds };
    }
    function overview() {
      const sf = latest.surface;
      set('runtime-ready', sf ? t(sf.Ready ? 'Ready' : 'Not ready') : t('Runtime counters unavailable'));
      set('runtime-node', sf ? sf.NodeID : '—');
      set('metric-inflight', sf ? fmt(sf.InFlight) : '—');
      const total = sum('dorang_requests_total'), errors = sum('dorang_requests_total', s => Number(s.labels.status) >= 400);
      set('metric-rps', history.length ? fmt(history[history.length - 1].r, 2) : '—');
      set('metric-errors', total ? fmt(errors / total * 100, 2) + '%' : '—');
      set('metric-latency', sf ? fmt(sf.AvgLatencyMS) + ' ms' : '—');
      const tokenRoot = $('#token-breakdown');
      if (tokenRoot) {
        tokenRoot.replaceChildren();
        for (const [kind,label] of [['input','Input'],['output','Output'],['cache_read','Cache read'],['cache_write','Cache write'],['reasoning','Reasoning']]) {
          const row = node('div'); row.append(node('span', t(label)), node('strong', fmt(sum('dorang_tokens_total', s => s.labels.kind === kind)))); tokenRoot.append(row);
        }
        const input = sum('dorang_tokens_total', s => s.labels.kind === 'input'), cache = sum('dorang_tokens_total', s => s.labels.kind === 'cache_read');
        const row = node('div'); row.append(node('span', t('Cache read / input')), node('strong', input ? fmt(cache / input * 100, 1) + '%' : '—')); tokenRoot.append(row);
      }
      drawChart(); renderModels(); renderRequests($('#recent-requests'), true);
    }
    function drawChart() {
      const max = Math.max(1, ...history.map(p => p.r)), points = history.map((p,i) => [40 + i / 59 * 705, 155 - p.r / max * 130]);
      const path = points.map((p,i) => (i ? 'L' : 'M') + p.join(',')).join(' ');
      $('#traffic-line').setAttribute('d', path);
      $('#traffic-area').setAttribute('d', points.length ? path + ' L' + points[points.length-1][0] + ',155 L40,155 Z' : '');
      $('#errors-line').setAttribute('d', history.map((p,i) => (i ? 'L' : 'M') + (40 + i / 59 * 705) + ',' + (155 - p.e / max * 130)).join(' '));
      set('chart-max',fmt(max,1));set('chart-note',history.length ? fmt(history.length) + ' ' + t('samples') : t('Collecting samples'));
    }
    function renderModels() {
      const body = $('#model-metrics'); if (!body) return;
      const models = new Map();
      for (const s of samples) {
        const name = s.labels.model; if (!name) continue;
        if (!models.has(name)) models.set(name,{name,requests:0,errors:0,tokens:{},cost:0,priced:0,unpriced:0,duration:0,count:0,ttft:0,ttftCount:0});
        const m = models.get(name);
        if (s.name === 'dorang_requests_total') {m.requests += s.value; if (Number(s.labels.status) >= 400) m.errors += s.value;}
        if (s.name === 'dorang_model_tokens_total') m.tokens[s.labels.kind] = s.value;
        if (s.name === 'dorang_model_cost_nano_total') m.cost = s.value / 1e9;
        if (s.name === 'dorang_model_pricing_requests_total') m[s.labels.pricing] = s.value;
        if (s.name === 'dorang_request_duration_seconds_sum') m.duration=s.value;
        if (s.name === 'dorang_request_duration_seconds_count') m.count=s.value;
        if (s.name === 'dorang_ttft_seconds_sum') m.ttft=s.value;
        if (s.name === 'dorang_ttft_seconds_count') m.ttftCount=s.value;
      }
      body.replaceChildren();
      [...models.values()].filter(m=>m.name.toLowerCase().includes(modelQuery)).sort((a,b)=>b.requests-a.requests).forEach(m=>{
        const tr=node('tr'), td=node('td'), link=node('a',m.name);link.href=root.dataset.base+'/requests?model='+encodeURIComponent(m.name);td.append(link);tr.append(td);
        for(const value of [fmt(m.requests),fmt(m.errors),m.count?fmt(m.duration/m.count*1000,1)+' ms':'—',m.ttftCount?fmt(m.ttft/m.ttftCount*1000,1)+' ms':'—',...['input','output','cache_read','cache_write','reasoning'].map(k=>m.tokens[k]===undefined?'—':fmt(m.tokens[k])),m.unpriced?(m.priced?money(m.cost)+' + ':'')+t('Unpriced')+' ('+fmt(m.unpriced)+')':money(m.cost)])tr.append(node('td',value,'num'));
        body.append(tr);
      });
      if(!body.children.length)emptyRow(body,11,'No model samples yet');
    }
    function renderRequests(body,compact=false){
      if(!body)return;body.replaceChildren();
      const list=(latest.requests||[]).filter(r=>(compact||Object.values(r).join(' ').toLowerCase().includes(requestQuery))&&(statusQuery==='all'||(statusQuery==='errors'?r.status>=400:r.status<400)));
      set('request-count',fmt(list.length)+' / '+fmt((latest.requests||[]).length));
      for(const r of list.slice(0,compact?8:256)){
        const tr=node('tr');tr.append(node('td',new Date(r.at).toLocaleTimeString(document.documentElement.lang),'mono'),node('td',r.model),node('td',r.provider||'—'));
        const st=node('td');st.append(badge(r.status));tr.append(st,node('td',fmt(r.latency_ms)+' ms','num'));
        if(!compact)for(const n of [r.ttft_ms,r.input,r.output])tr.append(node('td',fmt(n),'num'));
        tr.append(node('td',fmt(r.cache_read),'num'));
        if(!compact)for(const n of [r.cache_write,r.reasoning])tr.append(node('td',fmt(n),'num'));
        tr.append(node('td',r.priced?money(r.cost):t('Unpriced'),'num'));
        if(!compact){const td=node('td'),b=node('button',t('Inspect'),'link');b.type='button';b.addEventListener('click',()=>inspectRequest(r));td.append(b);tr.append(td);}
        body.append(tr);
      }
      if(!body.children.length)emptyRow(body,compact?7:13,'No requests in this window');
    }
    function inspectRequest(r){
      const out=$('#request-detail-content');out.replaceChildren();const dl=node('dl',undefined,'request-detail');
      const labels={id:'Request ID',model:'Model',provider:'Provider',deployment:'Deployment',credential:'Credential',key:'API key ID',team:'Team ID',user:'User ID',endpoint:'Endpoint',status:'Status',error:'Error',latency_ms:'Latency (ms)',ttft_ms:'TTFT (ms)',wait_ms:'Queue wait (ms)',input:'Input tokens',output:'Output tokens',cache_read:'Cache read tokens',cache_write:'Cache write tokens',reasoning:'Reasoning tokens',cost:'Billed cost',priced:'Priced',streamed:'Streamed'};
      for(const [key,label]of Object.entries(labels)){const val=r[key];dl.append(node('dt',t(label)),node('dd',typeof val==='boolean'?t(val?'Yes':'No'):String(val??'—')));}
      out.append(dl);if(r.key){const a=node('a',t('Search this key in history'),'button');a.href=root.dataset.base+'/history?errors_only=false&key_id='+encodeURIComponent(r.key);out.append(a);}
      $('#request-dialog').showModal();
    }
    function metricCategory(name){
      if(/token|cache|prefix/.test(name))return'tokens';if(/cost|budget|spend|price|notional|subscription/.test(name))return'cost';if(/quota|capacity|reservation/.test(name))return'capacity';if(/health|error|fail|drop|fallback|circuit|reject|substitution/.test(name))return'health';if(/request|duration|ttft|response/.test(name))return'traffic';return'runtime';
    }
    function renderMetrics(){
      const out=$('#metric-families');if(!out)return;const offsets=new Map([...out.querySelectorAll('details')].map(d=>[d.dataset.family,{open:d.open,scroll:$('.scroll',d)?.scrollTop||0}]));
      const groups=new Map();for(const s of samples){if(category!=='all'&&metricCategory(s.name)!==category)continue;if(!(s.key+' '+s.help).toLowerCase().includes(metricQuery))continue;if(!groups.has(s.family))groups.set(s.family,[]);groups.get(s.family).push(s);}
      set('metric-count',fmt([...groups.values()].reduce((n,rows)=>n+rows.length,0))+' '+t('series')+' · '+fmt(groups.size)+' '+t('families'));
      out.replaceChildren();let index=0;
      for(const [family,rows]of groups){const details=node('details',undefined,'panel metric-family');details.open=offsets.has(family)?offsets.get(family).open:metricOpen.has(family)||(metricQuery!==''&&groups.size<8)||(index++<3&&metricOpen.size===0);details.dataset.family=family;
        const summary=node('summary'),heading=node('div');heading.append(node('strong',family),node('small',rows[0].help));summary.append(heading,node('span',rows[0].type+' · '+rows.length,'tag'));details.append(summary);
        const wrap=node('div',undefined,'scroll'),table=node('table'),head=node('thead'),hr=node('tr');for(const label of ['Series / labels','Value','Per second'])hr.append(node('th',t(label)));head.append(hr);table.append(head);const body=node('tbody');
        for(const s of rows){const tr=node('tr');tr.append(node('td',s.key,'mono wrap'),node('td',Number.isFinite(s.value)?fmt(s.value,6):s.raw,'num'),node('td',rates.has(s.key)?fmt(rates.get(s.key),4):'—','num'));body.append(tr);}table.append(body);wrap.append(table);details.append(wrap);details.addEventListener('toggle',()=>{if(details.open)metricOpen.add(family);else metricOpen.delete(family);});out.append(details);if(offsets.has(family))wrap.scrollTop=offsets.get(family).scroll;
      }
      if(!groups.size)out.append(node('div',t('No matching metrics'),'empty-state'));
    }
    function credentials(){
      const out=$('#credential-cards');if(!out)return;out.replaceChildren();const list=(latest.credentials||[]).filter(c=>(c.provider_id+' '+c.credential_id).toLowerCase().includes(credentialQuery));set('credential-count',fmt(list.length)+' '+t('credentials'));
      for(const c of list){const card=node('article',undefined,'panel credential-card'),head=node('div',undefined,'section-head'),title=node('div');title.append(node('h2',c.credential_id),node('small',c.provider_id,'muted'));head.append(title,node('span',t(c.health||'unknown'),'status-badge '+(c.health==='healthy'?'is-ok':c.health==='unknown'?'':'is-error')));card.append(head);if(c.sample_scope)card.append(node('p',t(c.sample_scope),'scope-note'));
        const facts=node('dl',undefined,'credential-facts');for(const [label,value]of [['Requests',c.requests],['Failures',c.failures],['Circuit opens',c.sample_scope?'—':c.circuit_opens],['Consecutive failures',c.sample_scope?'—':c.consecutive_failures],['TTFT',c.ttft_ms+' ms'],['Latency',c.latency_ms+' ms'],['Tokens / sec',c.sample_scope?'—':fmt(c.tokens_per_sec,1)],['Cooldown until',c.unavailable_until||'—']])facts.append(node('dt',t(label)),node('dd',String(value)));card.append(facts);
        const quotas=node('div',undefined,'quota-windows');for(const q of c.quota||[]){const line=node('div',undefined,'quota-window'),h=node('div');h.append(node('strong',q.window+' / '+q.metric),node('span',q.limit?fmt(q.used_pct)+'%':t('Unlimited')));line.append(h);const meter=node('progress');meter.max=100;meter.value=Math.max(0,Math.min(100,q.used_pct));meter.setAttribute('aria-label',q.window+' '+q.metric);line.append(meter,node('small',fmt(q.used)+' / '+(q.limit?fmt(q.limit):'∞')+' · '+t(q.source)+' · '+t('Reset')+': '+(q.reset_at||'—')+(q.stale?' · '+t('Stale'):'')));quotas.append(line);}if(!(c.quota||[]).length)quotas.append(node('p',t('No quota windows reported'),'muted'));card.append(quotas);out.append(card);
      }if(!list.length)out.append(node('div',t('No credentials reported'),'empty-state'));
    }
    function capacity(){const c=latest.capacity;for(const k of ['waiting','reservations','grants','expired'])set('capacity-'+k,c?fmt(c[k[0].toUpperCase()+k.slice(1)]):'—');const body=$('#capacity-axes');if(!body)return;body.replaceChildren();for(const a of c&&c.Axes||[]){const tr=node('tr');for(const v of [a.Axis,a.Key||'—',fmt(a.InUse),a.Limit?fmt(a.Limit):t('Unlimited'),fmt(a.Waiting),a.Limit?fmt(a.InUse/a.Limit*100,1)+'%':'—'])tr.append(node('td',v));body.append(tr);}if(!body.children.length)emptyRow(body,6,c?'No capacity constraints reported':'Capacity unavailable');}
    function render(){if(!latest)return;set('last-updated',new Date(latest.at).toLocaleTimeString(document.documentElement.lang));const problems=$('#telemetry-problems');problems.hidden=!latest.problems.length;problems.textContent=latest.problems.map(t).join(' · ');switch(root.dataset.console){case'overview':overview();break;case'requests':renderRequests($('#live-request-rows'));break;case'metrics':renderMetrics();break;case'credentials':credentials();break;case'capacity':capacity();break;}}
    const bind=(id,fn)=>{const e=document.getElementById(id);if(e)e.addEventListener('input',()=>{fn(e.value.toLowerCase());render();});};
    bind('model-search',v=>modelQuery=v);bind('request-search',v=>requestQuery=v);bind('request-status',v=>statusQuery=v);bind('metric-search',v=>metricQuery=v);bind('metric-category',v=>category=v);bind('credential-search',v=>credentialQuery=v);if($('#request-search'))$('#request-search').value=requestQuery;
    function stop(){if(stream){stream.close();stream=null;}}
    function connect(){if(paused||document.hidden||stream)return;state('Connecting');stream=new EventSource(root.dataset.stream);stream.addEventListener('telemetry',event=>{try{const data=JSON.parse(event.data);samples=parseMetrics(data.prometheus||'');updateRate(data);latest=data;received=Date.now();state('Live','is-live');render();}catch(_){state('Invalid telemetry','is-error');}});stream.addEventListener('expired',()=>{stop();state('Session expired','is-error');});stream.onerror=()=>state('Reconnecting','is-error');}
    const pause=$('#live-pause');if(pause)pause.addEventListener('click',()=>{paused=!paused;pause.setAttribute('aria-pressed',String(paused));pause.textContent=t(paused?'Resume':'Pause');if(paused){stop();state('Paused');}else{previous=null;connect();}});
    document.addEventListener('visibilitychange',()=>{if(document.hidden){stop();state('Paused');}else{previous=null;connect();}});
    window.addEventListener('pagehide',stop);window.addEventListener('pageshow',connect);
    setInterval(()=>{if(!paused&&!document.hidden&&received&&Date.now()-received>10000)state('Stale · reconnecting','is-error');},2000);
    document.addEventListener('dorang:language',()=>{render();if(pause)pause.textContent=t(paused?'Resume':'Pause');state(streamLabel,streamClass);});
    const download=$('#download-metrics');if(download)download.addEventListener('click',()=>{if(!latest)return;const url=URL.createObjectURL(new Blob([latest.prometheus],{type:'text/plain'})),a=node('a');a.href=url;a.download='dorang-metrics.txt';a.click();setTimeout(()=>URL.revokeObjectURL(url),1000);});
    connect();
  });
})();
