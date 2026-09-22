const $ = (selector, scope = document) => scope.querySelector(selector);
const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
let state, currentPage = '', toastTimer, editKind, editId, testController;
let lifecycle = 'running';
let clearingLogs = false, refreshVersion = 0, activityPage = 1;
const titles = { overview: 'Vue d’ensemble', providers: 'Fournisseurs', aliases: 'Alias de modèles', playground: 'Terrain d’essai', activity: 'Activité', settings: 'Connexion & paramètres' };
const defaults = { openrouter: ['OpenRouter', 'https://openrouter.ai/api/v1'], openai: ['OpenAI', 'https://api.openai.com/v1'], anthropic: ['Claude · Anthropic', 'https://api.anthropic.com/v1'], custom: ['Fournisseur personnalisé', 'http://127.0.0.1:11434/v1'] };
async function api(url, method = 'GET', data) {
  const response = await fetch(url, { method, headers: { 'X-P4-Local': '1', ...(data ? { 'Content-Type': 'application/json' } : {}) }, ...(data ? { body: JSON.stringify(data) } : {}) });
  const result = await response.json();
  if (!response.ok) throw new Error(result.error?.message || `Erreur HTTP ${response.status}`);
  return result;
}
function toast(message) { $('#toast').textContent = message; $('#toast').classList.add('visible'); clearTimeout(toastTimer); toastTimer = setTimeout(() => $('#toast').classList.remove('visible'), 4200); }
function ready(p) { return p.enabled && (p.hasKey || p.kind === 'custom'); }
function activeAliases() { return state.aliases.filter(a => a.enabled && state.providers.some(p => p.id === a.providerId && ready(p))); }
function providerName(id) { return state.providers.find(p => p.id === id)?.name || 'Fournisseur supprimé'; }
const heading = (eyebrow, title, description, action = '') => `<div class="page-heading"><div><div class="eyebrow">${eyebrow}</div><h1>${title}</h1><p>${description}</p></div>${action}</div>`;
const empty = (symbol, title, description, action = '') => `<div class="empty"><div class="empty-symbol">${symbol}</div><h3>${title}</h3><p>${description}</p>${action}</div>`;
const addAliasButton = '<button class="button primary" data-action="add-alias"><span>＋</span> Créer un alias</button>';
function providerCards(providers) {
  return `<div class="provider-grid">${providers.map(p => `<article class="provider-card"><div class="provider-top"><span class="provider-logo ${esc(p.kind)}"><img src="/fournisseurs/${esc(p.kind)}.png" alt=""></span><div><h3>${esc(p.name)}</h3><small>${p.kind === 'anthropic' ? 'Anthropic natif + OpenAI' : p.kind === 'custom' ? 'API compatible OpenAI' : 'Chat Completions'}</small></div></div><div class="provider-url" title="${esc(p.baseUrl)}">${esc(p.baseUrl)}</div><div class="provider-bottom"><span class="badge ${!p.enabled ? 'disabled' : ready(p) ? '' : 'pending'}">${!p.enabled ? 'Désactivé' : ready(p) ? 'Configuré' : 'Clé à ajouter'}</span><button class="button secondary small" data-action="edit-provider" data-id="${esc(p.id)}">Configurer <span>↗</span></button></div></article>`).join('')}</div>`;
}
function aliasTable(aliases) {
  if (!aliases.length) return empty('⇄', 'Donnez vos propres noms aux modèles', 'Créez un alias, choisissez son fournisseur et le modèle réellement utilisé. Vos logiciels conservent le même nom de modèle.', addAliasButton);
  return `<div class="table-scroll"><table><thead><tr><th>MODÈLE LOCAL</th><th>MODÈLE RÉEL</th><th>FOURNISSEUR</th><th>ÉTAT</th><th></th></tr></thead><tbody>${aliases.map(a => {
    const p = state.providers.find(p => p.id === a.providerId); const available = a.enabled && p && ready(p);
    return `<tr><td><span class="row-name"><span class="row-icon">⇄</span><span class="mono">${esc(a.name)}</span></span></td><td class="mono"><span class="arrow">→</span>${esc(a.targetModel)}</td><td>${esc(providerName(a.providerId))}</td><td><span class="badge ${available ? '' : 'pending'}">${!a.enabled ? 'Désactivé' : available ? 'Actif' : 'À configurer'}</span></td><td><div class="actions"><button class="button ghost small" data-action="edit-alias" data-id="${esc(a.id)}">Modifier</button><button class="button ghost small" data-action="delete-alias" data-id="${esc(a.id)}" aria-label="Supprimer ${esc(a.name)}">×</button></div></td></tr>`;
  }).join('')}</tbody></table></div>`;
}
function totalLogPages() {
  return Math.max(1, Math.ceil((state.logTotal || 0) / 100));
}
function activityTable(logs) {
  if (!logs.length) return empty('≋', 'Votre prochaine requête apparaîtra ici', 'Retrouvez le routage, le temps de réponse et le statut de chaque appel. Le contenu des conversations n’est pas enregistré.');
  return `<div class="table-scroll"><table><thead><tr><th>HEURE</th><th>ALIAS → MODÈLE RÉEL</th><th>FOURNISSEUR</th><th>STATUT</th><th>DURÉE</th></tr></thead><tbody>${logs.map(l => `<tr><td class="muted">${esc(new Date(l.at).toLocaleTimeString('fr-CA'))}</td><td><span class="mono">${esc(l.alias)}</span><span class="arrow">→</span><span class="mono muted">${esc(l.target)}</span></td><td>${esc(l.provider)}</td><td class="${l.status === 200 ? 'activity-ok' : 'activity-error'}">${l.status === 200 ? '✓' : '×'} ${l.status}</td><td class="mono muted">${(l.durationMs / 1000).toFixed(2)} s</td></tr>`).join('')}</tbody></table></div>`;
}
function overview() {
  const configured = state.providers.filter(ready).length;
  const avg = state.stats.requests ? `${Math.round(state.stats.totalMs / state.stats.requests)} <small>ms</small>` : '—';
  return heading('VOTRE IA, SELON VOS RÈGLES', 'Tout commence en local.', 'Connectez vos fournisseurs d’IA et contrôlez précisément les modèles utilisés par vos applications.', '<a class="button secondary" href="#playground">▷ Tester une requête</a>') +
    `<section class="endpoint-card"><div><div class="eyebrow">VOTRE POINT D’ACCÈS UNIQUE</div><div class="endpoint-url">${esc(state.baseUrl)} <span class="tag">OPENAI COMPATIBLE</span></div><p>Une seule adresse à utiliser dans tous vos clients compatibles.</p></div><div class="actions"><button class="button secondary small" data-action="copy-url">⧉ Copier l’URL</button></div></section>` +
    `<section class="metrics"><div class="metric"><div class="metric-label">Fournisseurs configurés <span>⊞</span></div><div class="metric-value">${configured} <small>/ ${state.providers.length}</small></div><div class="metric-foot">OpenAI, Claude, OpenRouter et plus</div></div><div class="metric"><div class="metric-label">Alias prêts à l’emploi <span>⇄</span></div><div class="metric-value">${activeAliases().length}</div><div class="metric-foot">${state.aliases.length} alias enregistré${state.aliases.length > 1 ? 's' : ''}</div></div><div class="metric"><div class="metric-label">Requêtes de la session <span>↗</span></div><div class="metric-value">${state.stats.requests}</div><div class="metric-foot">${state.stats.successful} réussie(s) · ${state.stats.failed} erreur(s)</div></div><div class="metric"><div class="metric-label">Temps moyen <span>◷</span></div><div class="metric-value">${avg}</div><div class="metric-foot">Durée totale, streaming inclus</div></div></section>` +
    `<div class="section-heading"><h2>Vos fournisseurs <small>${state.providers.length} disponibles</small></h2><a class="text-button" href="#providers">Gérer les fournisseurs →</a></div>${providerCards(state.providers.slice(0, 3))}` +
    `<div class="section-heading"><h2>Alias de modèles</h2><a class="text-button" href="#aliases">Voir tous les alias →</a></div><section class="wide-card">${aliasTable(state.aliases.slice(0, 4))}</section>` +
    `<div class="guide"><section class="guide-card"><h3><span class="guide-number">01</span>Le nom change. Vous gardez le contrôle.</h3><p>Un alias peut rediriger vers un autre modèle, même chez un autre fournisseur. Exemple de correspondance :</p><div class="route-demo"><code>claude-opus-4</code><span>→</span><code>Qwen3.8-flash</code></div><p>Indiquez l’identifiant exact proposé par votre fournisseur.</p></section><section class="guide-card"><h3><span class="guide-number">02</span>Branchez votre application.</h3><p>Renseignez l’URL locale, copiez votre clé locale et choisissez l’un de vos alias comme modèle.</p><div class="route-demo"><span>Votre client</span> → <code>P4Relay API</code> → <span>Votre fournisseur</span></div><a class="text-button" href="#settings">Voir les informations de connexion →</a></section></div>`;
}
function providers() {
  return heading('CONNEXIONS', 'Vos fournisseurs.', 'Centralisez les services que vous utilisez. Ajoutez aussi Ollama ou toute API compatible OpenAI.', '<button class="button primary" data-action="add-provider">＋ Ajouter un fournisseur</button>') + providerCards(state.providers) +
    `<div class="info">Claude Code utilise le format Anthropic Messages. Les fournisseurs OpenAI/OpenRouter reçoivent une traduction Chat Completions ; un fournisseur Anthropic reçoit les messages natifs.</div><div class="notice">Les clés des fournisseurs restent côté serveur. Elles sont enregistrées en clair dans le fichier local <span class="mono">data/config.json</span>, exclu de Git. Ne partagez pas ce fichier.</div>`;
}
function aliases() {
  return heading('ROUTAGE DES MODÈLES', 'Vos noms. Vos modèles.', 'Le nom local est celui que vos applications voient. Le modèle réel est celui qui répond.', addAliasButton) +
    `<input id="alias-search" class="alias-search" type="search" placeholder="Rechercher un alias ou un modèle…" aria-label="Rechercher un alias"><section class="wide-card" id="alias-results">${aliasTable(state.aliases)}</section><div class="info">Le remplacement s’applique aux requêtes et au champ <span class="mono">model</span> des réponses, y compris en streaming. Les capacités et les coûts restent ceux du modèle réellement appelé.</div>`;
}
function playground() {
  const aliases = activeAliases();
  return heading('TERRAIN D’ESSAI', 'Vérifiez votre routage.', 'Envoyez une requête à votre API locale et consultez la réponse du modèle.') +
    `<div class="playground-grid"><form class="card" id="test-form"><h2>Nouvelle requête</h2><label class="field"><span>Modèle local</span><select id="test-model" required>${aliases.length ? aliases.map(a => `<option value="${esc(a.name)}">${esc(a.name)}</option>`).join('') : '<option value="">Aucun alias prêt à l’emploi</option>'}</select></label><div class="info" id="test-route"></div><label class="field"><span>Votre message</span><textarea id="test-prompt" required placeholder="Écrivez un message pour tester votre modèle…">Réponds en français : explique en une phrase ce qu’est une API.</textarea></label><label class="check"><input type="checkbox" id="test-stream" checked>Afficher la réponse en streaming</label><div class="actions"><button class="button primary" id="send-test" ${!aliases.length ? 'disabled' : ''}>▷ Envoyer la requête</button><button class="button secondary" id="stop-test" type="button" hidden>Arrêter</button></div><p class="error" id="test-error" role="alert"></p><p class="notice">Ce test appelle réellement le fournisseur et peut être facturé par celui-ci.</p>${!aliases.length ? '<a class="text-button" href="#aliases">Créer et configurer un alias →</a>' : ''}</form><section class="card"><h2>Réponse du modèle</h2><div class="response-meta"><span id="response-status">En attente d’une requête</span><span id="response-time"></span></div><div class="response-box response-placeholder" id="test-response" role="status" aria-live="polite">La réponse s’affichera ici, au fil de la génération.</div><details><summary>Voir la réponse JSON</summary><pre id="test-json">Aucune réponse pour le moment.</pre></details></section></div>`;
}
function networkSection() {
  const net = state.networkStatus;
  if (!net) return '<section class="card"><h2>Réseau et port</h2><p>Relancez le logiciel avec Demarrer.cmd pour charger les nouveaux paramètres réseau.</p></section><br>';
  return `<section class="card network-card"><h2>Réseau et port</h2><p>Choisissez les appareils autorisés à joindre votre API. Les changements s’appliquent au prochain lancement.</p>
    <form id="network-form">
      <label class="network-switch"><input id="network-lan" type="checkbox" role="switch" aria-describedby="network-warning" ${state.network.lanEnabled ? 'checked' : ''}><span class="switch-track" aria-hidden="true"></span><span>Accès au réseau local <strong id="network-mode" aria-hidden="true">${state.network.lanEnabled ? 'ON' : 'OFF'}</strong></span></label>
      <p class="notice" id="network-binding">${state.network.lanEnabled ? 'ON : toutes les interfaces IPv4 (0.0.0.0).' : 'OFF : uniquement cet ordinateur (127.0.0.1).'}</p>
      <div class="network-warning" id="network-warning"><strong>Attention à l’exposition réseau</strong><p>En mode ON, les appareils pouvant joindre ce PC peuvent appeler l’API avec votre clé locale et consommer vos crédits fournisseur. Les clés et les messages circulent en HTTP non chiffré. Toutes les interfaces IPv4 sont concernées, y compris les VPN.</p><p>Utilisez un réseau de confiance. N’ouvrez pas ce port sur Internet. Si Windows bloque la connexion, autorisez seulement le port choisi sur votre réseau privé dans le pare-feu. Le logiciel ne modifie pas le pare-feu.</p><p>L’interface, les réglages et le bouton de fermeture restent accessibles uniquement via 127.0.0.1 ou localhost sur ce PC.</p></div>
      <label class="field network-port"><span>Port de l’API</span><input id="network-port" type="number" min="1" max="65535" step="1" required value="${state.network.port}"><small>Entre 1 et 65535. Valeur par défaut : 7777.</small></label>
      ${net.portOverride !== null ? `<div class="notice">La variable PORT impose actuellement le port ${net.portOverride}. Le port enregistré ci-dessus sera utilisé après suppression de cette variable au lancement.</div>` : ''}
      <p>Écoute actuelle : <code>${esc(net.host)}:${net.port}</code> · ${net.lanEnabled ? 'Réseau activé' : 'Accès local uniquement'}</p>
      ${net.lanEnabled ? `<div class="network-addresses"><strong>Adresses API pour les autres appareils</strong>${net.lanUrls.length ? net.lanUrls.map(url => `<div class="field-row"><input aria-label="Adresse API réseau" readonly value="${esc(url)}"><button type="button" class="button secondary" data-action="copy-network-url" data-url="${esc(url)}">Copier</button></div>`).join('') : '<p>Aucune adresse IPv4 détectée. Vérifiez votre connexion réseau.</p>'}<p class="notice">Utilisez l’adresse de la carte réseau partagée avec votre autre appareil et la clé API locale. Pour Claude, retirez /v1 de cette adresse. Les adresses peuvent changer après une reconnexion.</p></div>` : ''}
      ${net.restartRequired ? `<div class="info" role="status">Redémarrage requis : fermez le logiciel puis relancez <strong>Demarrer.cmd</strong>. L’interface sera disponible sur <code>${esc(net.nextLocalUrl)}</code>. Mettez aussi à jour le port dans vos clients API. Le mode réseau actuel reste actif jusqu’à l’arrêt.</div>` : ''}
      <p class="error" id="network-error" role="alert"></p><button class="button primary" type="submit" id="save-network">Enregistrer les paramètres réseau</button>
    </form></section><br>`;
}
function settings() {
  const model = state.aliases[0]?.name || 'votre-alias';
  const sample = `from openai import OpenAI\n\nclient = OpenAI(\n    base_url=${JSON.stringify(state.baseUrl)},\n    api_key="VOTRE_CLE_LOCALE"\n)\n\nresponse = client.chat.completions.create(\n    model=${JSON.stringify(model)},\n    messages=[{"role": "user", "content": "Bonjour !"}]\n)\nprint(response.choices[0].message.content)`;
  return heading('CONNEXION', 'Branchez vos outils.', 'Ces informations permettent à vos applications d’utiliser la passerelle locale.') + networkSection() +
    `<section class="card"><h2>Claude Code Desktop</h2><p>Dans la configuration de passerelle de Claude Desktop, utilisez l’adresse ci-dessous, la même clé locale et l’un de vos alias.</p><div class="field-row"><input aria-label="URL pour Claude Code Desktop" readonly value="${esc(state.baseUrl.replace(/\/v1$/, ''))}"><button class="button secondary" data-action="copy-claude-url">Copier l’URL Claude</button></div><div class="info">Claude ajoute lui-même <span class="mono">/v1/messages</span> : son URL de base doit donc être saisie <strong>sans /v1</strong>. Textes, images et outils clients sont traduits vers OpenAI/OpenRouter. Avec un fournisseur Anthropic, les messages sont transmis au format natif.</div><p>Pour les fournisseurs autres qu’Anthropic, le comptage des tokens est estimatif ; le cache et les réglages de réflexion Anthropic ne sont pas transposés.</p></section><br>` +
    `<div class="settings-grid"><section class="card"><h2>Accès à l’API</h2><label class="field"><span>URL de base</span><div class="field-row"><input readonly value="${esc(state.baseUrl)}"><button class="button secondary" data-action="copy-url">Copier</button></div></label><label class="field"><span>Clé API locale</span><div class="field-row"><input type="password" readonly id="local-token" value="${esc(state.localToken)}"><button class="button secondary" data-action="copy-token">Copier</button></div><small>À utiliser à la place des clés de vos fournisseurs.</small></label><div class="actions"><button class="button ghost small" data-action="show-token">Afficher / masquer</button><button class="button ghost small" data-action="rotate-token">Renouveler la clé</button></div><div class="code-label">ROUTES DISPONIBLES</div><p><span class="pill-code">GET /v1/models</span></p><p><span class="pill-code">POST /v1/chat/completions</span></p><div class="info">Choisissez un client compatible <strong>OpenAI Chat Completions</strong>. Claude Code utilise aussi POST /v1/messages et POST /v1/messages/count_tokens. Les endpoints Responses, génération d’images et audio ne sont pas exposés.</div><p class="notice">Le mode réseau et le port se règlent ci-dessus. L’adresse locale reste utilisable sur ce PC.</p></section><section class="card"><h2>Exemple avec le SDK OpenAI</h2><p>Installez le paquet Python <span class="pill-code">openai</span>, puis remplacez la clé locale dans cet exemple.</p><div class="code-label">PYTHON</div><pre class="code" id="code-sample">${esc(sample)}</pre><button class="text-button" data-action="copy-example">Copier l’exemple →</button><div class="notice">L’interface et le routage fonctionnent sur votre PC. Les messages sont envoyés au fournisseur associé à l’alias ; choisissez un fournisseur local si vous souhaitez les traiter sur cet ordinateur.</div></section></div>`;
}
function render() {
  if (!state || lifecycle !== 'running') return;
  const wanted = location.hash.slice(1) || 'overview';
  const page = Object.hasOwn(titles, wanted) ? wanted : 'overview';
  if (testController && page !== 'playground') testController.abort();
  currentPage = page;
  document.querySelectorAll('[data-page]').forEach(link => { link.classList.toggle('active', link.dataset.page === page); if (link.dataset.page === page) link.setAttribute('aria-current', 'page'); else link.removeAttribute('aria-current'); });
  $('#breadcrumb').textContent = titles[page];
  $('#nav-providers').textContent = state.providers.length;
  $('#nav-aliases').textContent = state.aliases.length;
  const views = { overview, providers, aliases, playground, settings, activity: () => { const pages = totalLogPages(); const page = activityPage; return heading('JOURNAL LOCAL', 'Suivez chaque requête.', 'Derniers appels reçus par la passerelle, du plus récent au plus ancien. Aucun message ni réponse n’est conservé dans ce journal.', `<div class="actions"><button class="button secondary" data-action="refresh">↻ Actualiser</button><button class="button danger" data-action="clear-logs" ${clearingLogs || !state.logs.length ? 'disabled' : ''}>${clearingLogs ? 'Effacement…' : 'Effacer le journal'}</button></div>`) + `<section class="wide-card">${activityTable(state.logs)}${pages > 1 ? `<div class="log-pagination"><button class="button secondary small" data-action="log-page" data-id="${page - 1}"${page <= 1 ? ' disabled' : ''}>← Précédente</button><span>Page ${page} / ${pages} · ${state.logTotal || 0} entrées</span><button class="button secondary small" data-action="log-page" data-id="${page + 1}"${page >= pages ? ' disabled' : ''}>Suivante →</button></div>` : ''}</section>`; } };
  $('#main').innerHTML = views[page]();
  if (page === 'playground') updateTestRoute();
}
async function refresh(redraw = true) {
  if (lifecycle !== 'running' || clearingLogs) return;
  const version = ++refreshVersion;
  try {
    const latest = await api('/api/state');
    if (lifecycle !== 'running' || version !== refreshVersion) return;
    state = latest;
    if (currentPage === 'activity') {
      const result = await api(`/api/logs?page=${activityPage}`);
      state.logs = result.logs || [];
      state.logTotal = result.total || 0;
      if (activityPage > totalLogPages() && totalLogPages() >= 1) {
        activityPage = totalLogPages();
        const retry = await api(`/api/logs?page=${activityPage}`);
        state.logs = retry.logs || [];
        state.logTotal = retry.total || 0;
      }
    }
    $('#server-status').innerHTML = '<i></i>Serveur en ligne'; $('#server-status').classList.remove('offline');
    if (redraw) render();
  } catch (error) {
    if (lifecycle !== 'running' || version !== refreshVersion) return;
    $('#server-status').innerHTML = '<i></i>Serveur indisponible'; $('#server-status').classList.add('offline');
    if (!state) $('#main').innerHTML = empty('!', 'La passerelle est indisponible', 'Démarrez le serveur avec Demarrer.cmd, puis rechargez cette page.', '<button class="button secondary" data-action="refresh">Réessayer</button>');
    if (redraw) toast(error.message);
  }
}
async function clearLogs() {
  if (clearingLogs || lifecycle !== 'running') return;
  clearingLogs = true;
  ++refreshVersion;
  render();
  try {
    await api('/api/logs', 'DELETE');
    if (lifecycle !== 'running') return;
    state.logs = [];
    state.logTotal = 0;
    activityPage = 1;
    toast('Journal effacé.');
  } finally {
    clearingLogs = false;
    if (lifecycle === 'running' && currentPage === 'activity') render();
  }
}
async function shutdown() {
  if (lifecycle !== 'running') return;
  lifecycle = 'stopping';
  const button = $('#shutdown-button');
  button.disabled = true;
  $('.shutdown-label', button).textContent = 'Fermeture…';
  try {
    await api('/api/shutdown', 'POST', {});
    lifecycle = 'stopped';
    clearInterval(refreshTimer);
    clearTimeout(toastTimer);
    $('#toast').classList.remove('visible');
    testController?.abort();
    if ($('#editor').open) closeEditor();
    state = null;
    document.body.classList.add('server-stopped');
    $('#server-status').innerHTML = '<i></i>Serveur arrêté';
    $('#server-status').classList.add('offline');
    $('#breadcrumb').textContent = 'Logiciel fermé';
    $('.shutdown-label', button).textContent = 'Logiciel fermé';
    $('.local-note').innerHTML = '<span class="tiny-dot"></span> Serveur arrêté';
    document.querySelectorAll('.sidebar a').forEach(link => { link.setAttribute('aria-disabled', 'true'); link.setAttribute('tabindex', '-1'); link.classList.remove('active'); });
    $('#main').innerHTML = '<section class="card shutdown-card" role="status"><span class="shutdown-symbol" aria-hidden="true">⏻</span><h1>Le logiciel est fermé.</h1><p>Le serveur local est arrêté. Vos fournisseurs, clés et alias sont enregistrés.</p><p>Vous pouvez fermer cet onglet. Pour relancer P4Relay API, double-cliquez sur <strong>P4Relay.exe</strong>.</p></section>';
    $('#main').focus();
  } catch (error) {
    lifecycle = 'running';
    button.disabled = false;
    $('.shutdown-label', button).textContent = 'Fermer le logiciel';
    toast(`L’arrêt n’a pas pu être confirmé : ${error.message}`);
  }
}
function openEditor(kind, id) {
  editKind = kind; editId = id;
  $('#dialog-error').textContent = ''; $('#save-dialog').disabled = false;
  $('#dialog-title').textContent = `${id ? 'Modifier' : 'Ajouter'} ${kind === 'provider' ? 'un fournisseur' : 'un alias'}`;
  if (kind === 'provider') {
    const p = state.providers.find(p => p.id === id) || { kind: 'custom', name: '', baseUrl: '', enabled: true };
    $('#dialog-fields').innerHTML = `<label class="field"><span>Type de fournisseur</span><select name="kind" id="provider-kind">${Object.entries(defaults).map(([key, val]) => `<option value="${key}" ${p.kind === key ? 'selected' : ''}>${esc(val[0])}</option>`).join('')}</select></label><label class="field"><span>Nom</span><input name="name" required maxlength="256" value="${esc(p.name)}" placeholder="Mon fournisseur"></label><label class="field"><span>URL de base</span><input name="baseUrl" type="url" required value="${esc(p.baseUrl)}" placeholder="https://api.exemple.com/v1"><small>Inclure /v1 si requis. Pour Ollama : http://127.0.0.1:11434/v1</small></label><label class="field"><span>Clé API du fournisseur</span><input name="apiKey" type="password" autocomplete="new-password" placeholder="${p.hasKey ? 'Clé enregistrée · laisser vide pour la conserver' : 'Collez votre clé API'}"><small>Facultative uniquement pour les fournisseurs personnalisés sans authentification.</small></label>${p.hasKey ? '<label class="check"><input type="checkbox" name="clearKey">Effacer la clé enregistrée</label>' : ''}<label class="field" id="workspace-field"><span>Workspace Anthropic (facultatif)</span><input name="workspaceId" value="${esc(p.workspaceId || '')}" placeholder="Identifiant de workspace si votre clé l’exige"></label><label class="check"><input type="checkbox" name="enabled" ${p.enabled ? 'checked' : ''}>Fournisseur activé</label><p class="provider-editor-note">Clé conservée dans <span class="mono">data/config.json</span>, en clair sur ce PC. Elle n’est jamais renvoyée par l’interface.</p>${id ? `<button class="text-button" type="button" data-action="delete-provider" data-id="${esc(id)}">Supprimer ce fournisseur</button>` : ''}`;
    $('#workspace-field').hidden = p.kind !== 'anthropic';
  } else {
    const a = state.aliases.find(a => a.id === id) || { name: '', targetModel: '', providerId: state.providers.find(ready)?.id || state.providers[0]?.id, enabled: true };
    $('#dialog-fields').innerHTML = `<label class="field"><span>Nom du modèle local (alias)</span><input name="name" required maxlength="256" value="${esc(a.name)}" placeholder="Exemple : claude-opus-4"><small>C’est le nom utilisé dans vos applications clientes.</small></label><label class="field"><span>Fournisseur à appeler</span><select name="providerId" id="alias-provider" required>${state.providers.map(p => `<option value="${esc(p.id)}" ${a.providerId === p.id ? 'selected' : ''}>${esc(p.name)}${ready(p) ? '' : ' · à configurer'}</option>`).join('')}</select></label><label class="field"><span>Identifiant exact du modèle réel</span><div class="field-row"><input name="targetModel" id="target-model" required maxlength="256" list="model-list" value="${esc(a.targetModel)}" placeholder="Exemple : Qwen3.8-flash"><button class="button secondary small" type="button" data-action="load-models">↻ Charger</button></div><datalist id="model-list"></datalist><small>Chargez les modèles du fournisseur ou saisissez son identifiant exact.</small><span id="models-status" class="inline-status" role="status"></span></label><label class="check"><input type="checkbox" name="enabled" ${a.enabled ? 'checked' : ''}>Alias activé</label><div class="info">L’alias est libre. Le modèle réel doit exister chez le fournisseur choisi. Le nom seul ne transforme pas les capacités du modèle.</div>`;
  }
  $('#editor').showModal();
}
function closeEditor() { $('#editor').close(); }
$('#editor').addEventListener('close', () => { $('#dialog-fields').innerHTML = ''; });
$('#close-dialog').addEventListener('click', closeEditor); $('#cancel-dialog').addEventListener('click', closeEditor);
$('#editor-form').addEventListener('submit', async event => {
  event.preventDefault(); const form = new FormData(event.target); const input = Object.fromEntries(form.entries());
  input.enabled = form.has('enabled'); input.clearKey = form.has('clearKey'); if (editId) input.id = editId;
  $('#save-dialog').disabled = true; $('#dialog-error').textContent = '';
  try { await api(editKind === 'provider' ? '/api/providers' : '/api/aliases', 'POST', input); closeEditor(); toast('Configuration enregistrée.'); await refresh(); }
  catch (error) { $('#dialog-error').textContent = error.message; }
  finally { $('#save-dialog').disabled = false; }
});
document.addEventListener('change', event => {
  if (event.target.id === 'network-lan') {
    $('#network-mode').textContent = event.target.checked ? 'ON' : 'OFF';
    $('#network-binding').textContent = event.target.checked ? 'ON : toutes les interfaces IPv4 (0.0.0.0).' : 'OFF : uniquement cet ordinateur (127.0.0.1).';
  }
  if (event.target.id === 'provider-kind') {
    const kind = event.target.value; const form = $('#editor-form'); form.elements.name.value = defaults[kind][0]; form.elements.baseUrl.value = defaults[kind][1]; $('#workspace-field').hidden = kind !== 'anthropic';
  }
  if (event.target.id === 'alias-provider') { $('#model-list').innerHTML = ''; $('#models-status').textContent = ''; }
  if (event.target.id === 'test-model') updateTestRoute();
});
document.addEventListener('input', event => {
  if (event.target.id === 'alias-search') { const q = event.target.value.toLowerCase(); $('#alias-results').innerHTML = aliasTable(state.aliases.filter(a => `${a.name} ${a.targetModel} ${providerName(a.providerId)}`.toLowerCase().includes(q))); }
});
document.addEventListener('click', async event => {
  if (lifecycle !== 'running') { if (event.target.closest('.sidebar a, [data-action]')) event.preventDefault(); return; }
  const button = event.target.closest('[data-action]'); if (!button) return;
  const { action, id } = button.dataset;
  try {
    if (action === 'shutdown') { await shutdown(); return; }
    if (action === 'add-provider' || action === 'edit-provider') openEditor('provider', id);
    if (action === 'add-alias' || action === 'edit-alias') { if (!state.providers.length) return toast('Ajoutez d’abord un fournisseur.'); openEditor('alias', id); }
    if (action === 'copy-url') { await navigator.clipboard.writeText(state.baseUrl); toast('URL locale copiée.'); }
    if (action === 'copy-network-url') { await navigator.clipboard.writeText(button.dataset.url); toast('Adresse API réseau copiée.'); }
    if (action === 'copy-claude-url') { await navigator.clipboard.writeText(state.baseUrl.replace(/\/v1$/, '')); toast('URL pour Claude Code copiée.'); }
    if (action === 'copy-token') { await navigator.clipboard.writeText(state.localToken); toast('Clé locale copiée.'); }
    if (action === 'copy-example') { await navigator.clipboard.writeText($('#code-sample').textContent); toast('Exemple copié.'); }
    if (action === 'show-token') $('#local-token').type = $('#local-token').type === 'password' ? 'text' : 'password';
    if (action === 'refresh') await refresh();
    if (action === 'clear-logs') await clearLogs();
  if (action === 'log-page' && !clearingLogs) {
    const target = parseInt(id, 10);
    if (target >= 1 && target <= totalLogPages() && target !== activityPage) {
      activityPage = target;
      await refresh();
    }
  }
    if (action === 'rotate-token' && confirm('Renouveler la clé locale ? Les applications devront utiliser la nouvelle clé.')) { await api('/api/token/rotate', 'POST', {}); await refresh(); toast('Nouvelle clé locale créée.'); }
    if (action === 'delete-alias' && confirm('Supprimer cet alias ?')) { await api(`/api/aliases/${encodeURIComponent(id)}`, 'DELETE'); await refresh(); toast('Alias supprimé.'); }
    if (action === 'delete-provider' && confirm('Supprimer ce fournisseur et sa clé enregistrée ?')) { await api(`/api/providers/${encodeURIComponent(id)}`, 'DELETE'); closeEditor(); await refresh(); toast('Fournisseur supprimé.'); }
    if (action === 'load-models') {
      const providerId = $('#alias-provider').value; const status = $('#models-status');
      button.disabled = true; status.textContent = 'Chargement des modèles…';
      try {
        const result = await api(`/api/providers/${encodeURIComponent(providerId)}/models`);
        if ($('#alias-provider')?.value === providerId) { $('#model-list').innerHTML = result.data.map(m => `<option value="${esc(m.id)}">${esc(m.name)}</option>`).join(''); status.textContent = `${result.data.length} modèles chargés. Saisissez quelques lettres pour filtrer.`; }
      } catch (error) { status.textContent = error.message; }
      finally { button.disabled = false; }
    }
  } catch (error) { if ($('#editor').open) $('#dialog-error').textContent = error.message; else toast(error.message); }
});
function updateTestRoute() {
  const a = state.aliases.find(a => a.name === $('#test-model')?.value);
  $('#test-route').innerHTML = a ? `<span class="mono">${esc(a.name)}</span> <span class="arrow">→</span> ${esc(providerName(a.providerId))}<br><span class="mono">${esc(a.targetModel)}</span>` : 'Configurez un fournisseur et créez un alias pour commencer.';
}
document.addEventListener('submit', async event => {
  if (event.target.id === 'network-form') {
    event.preventDefault();
    if (lifecycle !== 'running') return;
    const button = $('#save-network'), errorBox = $('#network-error');
    const input = { lanEnabled: $('#network-lan').checked, port: Number($('#network-port').value) };
    button.disabled = true; errorBox.textContent = '';
    try {
      await api('/api/network', 'POST', input);
      if (lifecycle !== 'running') return;
      await refresh(); toast('Paramètres réseau enregistrés. Ils seront appliqués au prochain lancement.');
    } catch (error) { errorBox.textContent = error.message; }
    finally { button.disabled = false; }
    return;
  }
  if (event.target.id !== 'test-form') return;
  event.preventDefault(); if (testController) return;
  const controller = new AbortController(); testController = controller;
  const output = $('#test-response'), errorBox = $('#test-error'), status = $('#response-status'), elapsed = $('#response-time'), raw = $('#test-json');
  const send = $('#send-test'), stop = $('#stop-test');
  output.textContent = ''; output.classList.remove('response-placeholder'); errorBox.textContent = ''; raw.textContent = ''; status.textContent = 'Connexion au fournisseur…'; elapsed.textContent = '';
  send.disabled = true; stop.hidden = false; stop.onclick = () => controller.abort();
  const started = performance.now(); const stream = $('#test-stream').checked; const rawEvents = [];
  try {
    const response = await fetch('/v1/chat/completions', { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${state.localToken}` }, signal: controller.signal,
      body: JSON.stringify({ model: $('#test-model').value, messages: [{ role: 'user', content: $('#test-prompt').value }], stream }) });
    if (!response.ok) { const data = await response.json(); throw new Error(data.error?.message || `Erreur ${response.status}`); }
    if (stream) {
      status.textContent = 'Réception en cours…';
      const reader = response.body.getReader(); const decoder = new TextDecoder(); let buffer = ''; let doneSeen = false;
      function consume(line) {
        if (!line.startsWith('data:')) return;
        const text = line.slice(5).trim(); if (!text) return; if (text === '[DONE]') { doneSeen = true; return; }
        const data = JSON.parse(text); if (data.error) throw new Error(data.error.message || 'Erreur du fournisseur.');
        if (rawEvents.length < 500) rawEvents.push(data);
        const delta = data.choices?.[0]?.delta;
        if (typeof delta?.content === 'string') output.textContent += delta.content;
      }
      while (true) {
        const part = await reader.read();
        buffer += part.done ? decoder.decode() : decoder.decode(part.value, { stream: true });
        let index;
        while ((index = buffer.indexOf('\n')) >= 0) { consume(buffer.slice(0, index).replace(/\r$/, '')); buffer = buffer.slice(index + 1); }
        if (part.done) { if (buffer.trim()) consume(buffer); break; }
      }
      raw.textContent = JSON.stringify(rawEvents, null, 2);
      if (!doneSeen) throw new Error('Le flux s’est terminé sans signal de fin. La réponse peut être incomplète.');
    } else {
      const data = await response.json(); output.textContent = data.choices?.[0]?.message?.content || '(Réponse sans texte : consultez le JSON.)'; raw.textContent = JSON.stringify(data, null, 2);
    }
    if (!output.textContent) output.textContent = '(Réponse sans texte : consultez le JSON.)';
    status.textContent = '✓ Réponse reçue';
  } catch (error) { status.textContent = controller.signal.aborted ? 'Requête arrêtée' : 'Échec de la requête'; if (!controller.signal.aborted) errorBox.textContent = error.message; }
  finally { elapsed.textContent = `${((performance.now() - started) / 1000).toFixed(2)} s`; send.disabled = false; stop.hidden = true; testController = null; await refresh(false); }
});
window.addEventListener('hashchange', render);
refresh();
const refreshTimer = setInterval(() => { if (!$('#editor').open && !testController) refresh(currentPage === 'activity'); }, 15000);
