# P4Relay

Passerelle IA locale : expose un point d'entrée unique vers vos fournisseurs (OpenRouter, OpenAI, Anthropic, personnalisés) avec alias de modèles, gestion des clés et interface web intégrée.

## Fonctionnalités

- **Multi-fournisseurs** : OpenRouter, OpenAI, Anthropic + fournisseurs personnalisés
- **Alias de modèles** : mappez un nom court vers un modèle/fournisseur cible
- **Interface web** : dashboard intégré (statut, fournisseurs, alias, terrain d'essai, activité, paramètres)
- **Réseau** : localhost par défaut, option LAN
- **Sécurité** : token local, clés stockées localement et jamais renvoyées par l'interface

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
public/         Interface web (index.html, app.js, style.css, logo.png)
data/           Configuration locale (ignorée par git — contient les clés)
```

## Configuration

Le fichier `data/config.json` (créé au premier lancement, **ignoré par git**) contient :
- le token local d'authentification
- les fournisseurs et leurs clés API
- les alias de modèles
- les réglages réseau (port, LAN)

> ⚠️ Les clés API sont stockées en clair sur le PC et ne doivent jamais être committées.
