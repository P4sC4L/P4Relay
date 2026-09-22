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

### Chiffrement des clés API (optionnel)

Par défaut, les clés API sont stockées en clair dans `data/config.json`.
Pour les chiffrer au repos (AES-GCM), définissez la variable d'environnement
`P4RELAY_MASTER_KEY` avant le lancement : une chaîne de **64 caractères hexadécimaux**
(32 octets), par exemple :

```
set P4RELAY_MASTER_KEY=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
P4Relay.exe
```

- Les clés déjà enregistrées en clair sont **chiffrées automatiquement** au premier démarrage avec la clé définie.
- La clé maîtresse n'est stockée nulle part : sans elle, le serveur **refuse de démarrer** si la configuration contient des clés chiffrées.
- Un changement de clé maîtresse rend les clés chiffrées illisibles (elles doivent alors être ressaisies).
