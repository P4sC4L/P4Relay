# P4Relay

Passerelle IA locale : expose un point d'entrée unique vers vos fournisseurs (OpenRouter, OpenAI, Anthropic, personnalisés) avec alias de modèles, gestion des clés et interface web intégrée.

## Fonctionnalités

- **Multi-fournisseurs** : OpenRouter, OpenAI, Anthropic + fournisseurs personnalisés
- **Alias de modèles** : mappez un nom court vers un modèle/fournisseur cible
- **Interface web** : dashboard intégré (statut, fournisseurs, alias, terrain d'essai, activité, paramètres)
- **Réseau** : localhost par défaut, option LAN
- **Sécurité** : token local, chiffrement AES-GCM des clés API au repos (clé maîtresse), clés jamais renvoyées par l'interface

## Demo
![Screenshot de P4Relay sur la vue d'ensemble.](https://raw.githubusercontent.com/P4sC4L/P4Relay/main/public/screenshot/demo.png)

## Compilation

```
go build -o P4Relay.exe .
```

## Lancement

```
P4Relay.exe
```

Le serveur tourne sur `http://127.0.0.1:7777` par défaut.

## Structure

```
main.go         Serveur HTTP, routes, logique réseau
anthropic.go    Adaptateur Anthropic
config.go       Chargement/sauvegarde de la configuration
errors.go       Gestion des erreurs
crypto.go     Chiffrement AES-GCM des clés API (clé maîtresse)
public/         Interface web (index.html, app.js, style.css, logo.png)
data/           Configuration locale (ignorée par git — contient les clés)
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

1. variable d'environnement `P4RELAY_MASTER_KEY` (priorité, clé externe) ;
2. sinon, le fichier `data/key`, **généré automatiquement** à la première
   ouverture ;
3. sinon, une clé est générée et écrite dans `data/key`.

```
# Optionnel : fournir sa propre clé maîtresse (64 caractères hexadécimaux)
set P4RELAY_MASTER_KEY=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
P4Relay.exe
```

- Les clés déjà enregistrées en clair sont **chiffrées automatiquement** au
  premier démarrage.
- Le fichier `data/key` est séparé de `data/config.json` et ignoré par git.
  Tant que l'un des deux manque, les clés chiffrées restent illisibles.
- Un changement de clé maîtresse rend les clés chiffrées illisibles (elles
  doivent alors être ressaisies).
