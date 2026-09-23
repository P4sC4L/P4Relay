# P4Relay

Passerelle IA locale : expose un point d'entrée unique vers vos fournisseurs (OpenRouter, OpenAI, Anthropic, personnalisés) avec alias de modèles, gestion des clés et interface web intégrée.

## Fonctionnalités

- **Multi-fournisseurs** : OpenRouter, OpenAI, Anthropic + fournisseurs personnalisés
- **Alias de modèles** : mappez un nom court vers un modèle/fournisseur cible
- **Interface web** : dashboard intégré (statut, fournisseurs, alias, terrain d'essai, activité, paramètres)
- **Réseau** : localhost par défaut, option LAN
- **Sécurité** : token local, chiffrement AES-GCM des clés API au repos (clé maîtresse), clés jamais renvoyées par l'interface

## Demo
![Screenshot de P4Relay sur la vue d'ensemble.](https://raw.githubusercontent.com/P4sC4L/P4Relay/main/internal/p4relay/web/public/screenshot/demo.png)

## Compilation

```
go build -o P4Relay.exe ./cmd/p4relay
```

## Lancement

```
P4Relay.exe
```

Le serveur tourne sur `http://127.0.0.1:7777` par défaut.

## Structure

```
cmd/p4relay/                 Point d'entrée (main.go, ressource d'icône Windows)
internal/p4relay/
  server/                    Passerelle, routeur HTTP, sécurité d'accès
  proxy/                     Routage des requêtes /v1 vers les fournisseurs
  admin/                     Routes d'administration /api/*
  anthropic/                 Conversion OpenAI ↔ Anthropic, flux SSE
  config/                    Chargement/sauvegarde de la configuration
  journal/                   Journal d'activité et statistiques
  errors/                    Erreurs API et utilitaires JSON
  crypto/                    Chiffrement AES-GCM des clés API (clé maître)
  web/                       Interface embarquée (index.html, app.js, style.css, logos)
data/                        Configuration locale (ignorée par git — contient les clés)
```

## Configuration

Le fichier `data/config.json` (créé au premier lancement, **ignoré par git**) contient :
- le token local d'authentification
- les fournisseurs et leurs clés API
- les alias de modèles
- les réglages réseau (port, LAN)

> ⚠️ `data/config.json` ne doit jamais être committé.

### Chiffrement des clés API

Les clés API sont **chiffrées au repos par défaut** (AES-256-GCM) dans
`data/config.json`. La clé maîtresse (32 octets) est résolue ainsi :

1. variable d'environnement `P4RELAY_MASTER_KEY` ;
2. sinon, le fichier `data/key`, **généré automatiquement** à la première
   ouverture.

Les clés déjà enregistrées en clair sont **chiffrées automatiquement** au
premier démarrage. Le fichier `data/key` est ignoré par git.

#### ⚠️ Le fichier `data/key` n'est pas un endroit sécurisé

`data/key` contient la clé maîtresse **en clair**, dans le même dossier que
`config.json` : il suffit de copier le dossier pour avoir les deux (sauvegarde,
compte local, malware), et avec les deux fichiers, une clé API se déchiffre
**instantanément**. Le chiffrement ne protège que `config.json` **seul**
(brute-force 2²⁵⁶, irréaliste).

**Recommandation : stocker la clé ailleurs** (coffre de mots de passe, autre
machine) et l'injecter via la variable d'environnement, qui n'est jamais
écrite sur disque :

```
# 64 caractères hexadécimaux — la clé reste hors machine
set P4RELAY_MASTER_KEY=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
P4Relay.exe
```

Quand la variable est définie, `data/key` n'est ni lu ni créé. Pour un
**redémarrage ou un changement de machine**, il suffit de redéfinir **la même
clé** : les `config.json` chiffrés restent déchiffrables partout où elle est
connue, et illisibles avec une clé différente (les clés doivent alors être
ressaisies).
