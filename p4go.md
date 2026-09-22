# P4GO — P4Relay : récapitulatif, points restants, passation

> Ce fichier est la mémoire de travail du projet. **À mettre à jour en fin de chaque session.**
> Dernière mise à jour : **2026-09-22** (session de migration `p4localapi.exe` → `P4Relay.exe` + journal persistant).

---

## 1. Le projet en une phrase

**P4Relay** est une passerelle IA locale (port Go du projet P4GO) : un serveur HTTP local (port **7777**) qui expose une API compatible OpenAI (`/v1/chat/completions`), route les alias de modèles vers des fournisseurs (OpenRouter, OpenAI, Anthropic, custom/Ollama), avec une interface web d'administration.

- Repo GitHub : **https://github.com/P4sC4L/P4Relay** (branche `main`, public)
- Exécutable : **`P4Relay.exe`** (module Go `p4relay`) — **l'ancien nom `p4localapi.exe` n'existe plus** (migration terminée le 22/09)
- Front embarqué dans l'exe via `embed` (dossier `public/`), servi par la map statique `staticFiles` de `main.go`
- Config locale : `data/config.json` (jamais committée) ; journal : `data/journal.log`
- Build (si un jour autorisé) : `C:\ProgramData\ajean\workspace\go\bin\go.exe build -o P4Relay.exe .`
- **⚠️ Consigne en vigueur : ne plus JAMAIS recompiler sans demande explicite de l'utilisateur.**

## 2. Travaux effectués (état au 22/09)

### Session 22/09 (dernière)
- **Migration de nom complète** `p4localapi.exe` → `P4Relay.exe` : plus aucune référence à l'ancien nom dans le code, le front, les scripts ou la mémoire.
- **Journal d'activité persistant** (commit `4bf815a` — poussé sur GitHub) :
  - `data/journal.log` (JSON) : rechargé au démarrage (`loadJournal`), réécrit à chaque requête (`record` → `saveJournal`), supprimé par « Effacer le journal » (`DELETE /api/logs`).
  - `GET /api/logs?page=N` : pagination 100 entrées/page (le plus récent en premier, page 1).
  - `/api/state` : renvoie toujours les 100 dernières + `stats`.
  - Front (onglet Activité) : pagination « Précédente / Suivante », compteur « Page X / Y · N entrées », bouton danger avec états hover/désactivé propres.
  - **Fix BOM** : `loadJournal` supprime un BOM UTF-8 (EF BB BF) avant `json.Unmarshal` (leçon : PowerShell 5.1 `Set-Content -Encoding UTF8` ajoute un BOM qui casse Go).
- **Règles de commit** : messages sans préfixe « P4Relay : », description de la modification uniquement.
- **Règle compilation** : ne plus recompiler (consigne utilisateur du 22/09).

### Sessions précédentes (22/09, plus tôt)
- Repo **refait de zéro** sur GitHub (histoire propre, 5 commits initiaux) ; ancienne histoire conservée en local sur branche `backup-old-history` (non poussée).
- Clé SSH : **`id_ed25519_p4relay_new`** (deploy key write) ; `core.sshCommand` configuré dans le repo.
- Logo + favicon `icon.png` (route statique ajoutée dans `staticFiles`).
- Section **demo** + screenshot dans le README.
- `LICENSE` GPL-3.0, `README.md` rédigé.
- Ancienne corruption mojibake (commit `28a06bd`, encodage PowerShell) corrigée — voir règle encodage §3.

### Historique fonctionnel du projet
- Port Go de la source Node de référence (`C:\Users\P4sC4L\Documents\ChatGPT\P4 LOCAL APIGO`).
- Fichiers Go : `main.go`, `config.go`, `anthropic.go`, `errors.go` ; front : `public/` (`index.html`, `app.js`, `style.css`, `icon.png`).
- Fonctionnalités : fournisseurs multiples + clés chiffrées, alias de modèles, playground/terrain d'essai, journal d'activité, stats, token local (rotation), gestion du port, `/health`.

## 3. Points restant à traiter

1. **Branche `backup-old-history`** (locale, jamais poussée) : à supprimer si l'ancienne histoire n'est plus utile.
2. **`data/journal.log`** contient ~250 entrées de test : à vider via le bouton « Effacer le journal » une fois les tests terminés (ou laisser, c'est ignoré par git).
3. **Version** : `version = "1.1.0"` dans `main.go` — à incrémenter au prochain vrai changement (nécessiterait une recompilation → demander).
4. **`staticFiles`** : tout nouveau fichier ajouté dans `public/` doit être enregistré dans la map (fichier + MIME) dans `main.go` + recompilation, sinon 404.
5. **Branching / CI** : rien de configuré (pas de CI, pas de tags de release). À envisager si le projet grandit.
6. **Sécurisation** : le token local protège l'admin ; vérifier que rien d'autre n'est exposé au-delà de 127.0.0.1 si le port est ouvert.

## 4. Règles & leçons à ne jamais oublier

- **NOM** : exécutable = `P4Relay.exe`, marque = `P4Relay` (collé, sans espace). Jamais `p4localapi`, jamais « P4 Relay ».
- **NE JAMAIS recompilier** sans demande explicite (consigne 22/09).
- **NE JAMAIS** `Set-Content -Encoding UTF8` / `Out-File` PowerShell sur un fichier Go (BOM + double-encodage des accents = corruption). Utiliser l'outil `edit` (UTF-8 sans BOM) ou `git checkout <commit> -- file` pour restaurer.
- **Commits** : description seule, sans préfixe.
- **`p4go.md`** : mettre à jour en fin de chaque session (ce fichier).
- **Mémoire projet** : pages `p4relay-github.md` (dépôt, règles, leçons) et `p4localapi-go-port.md` (port Go) — à consulter.

## 5. Passation de pouvoir (pour la session suivante)

**État actuel** :
- Serveur `P4Relay.exe` **en marche** sur le port 7777 (PID à vérifier : `Get-Process P4Relay`).
- Repo GitHub **à jour** (HEAD `4bf815a`), arborescence propre (pas de fichiers orphelins committables ; `data/`, `*.exe`, `*.log` ignorés).
- Front + journal persistant : opérationnels et testés (250 entrées de test survivent aux redémarrages).

**Premiers gestes à faire** :
1. `git status` dans `C:\ProgramData\ajean\workspace\p4go` — doit être propre.
2. `Get-Process P4Relay` — si le serveur est arrêté, relancer : `C:\ProgramData\ajean\workspace\p4go\P4Relay.exe` (en tâche de fond, logs dans `server.log`).
3. Tester : `http://127.0.0.1:7777/` (admin, avec token local) et `http://127.0.0.1:7777/health`.
4. Lire ce fichier + les pages mémoire `p4relay-github.md` et `p4localapi-go-port.md` avant toute modification.
5. Si une modification du code est demandée : **demander avant de compiler** (règle §4), puis commit (message sans préfixe) + push, puis mettre à jour ce fichier.

**Détails techniques de dépannage** :
- Go (si besoin un jour) : `C:\ProgramData\ajean\workspace\go\bin\go.exe` (pas dans le PATH).
- Fichier `main.go` en **CRLF** : l'outil `edit` peut échouer sur des snippets multi-lignes → passer par un script (node/python) avec `\r\n` explicites, ou un edit monoligne.
- PowerShell 5.1 : le `?` des URLs est mangé dans les commandes → utiliser `curl.exe` ou échapper ; éviter `Set-Content -Encoding UTF8` sur du Go.
- Vérifier un push : `git log --oneline -3` local doit commencer par le même commit que `git ls-remote origin main`.
