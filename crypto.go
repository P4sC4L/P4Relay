package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// Chiffrement AES-GCM des clés API (champ apiKey) dans data/config.json.
//
// La clé maîtresse (32 octets, 64 caractères hexadécimaux) est fournie via la
// variable d'environnement P4RELAY_MASTER_KEY. Elle n'est JAMAIS stockée dans
// le code ni dans le fichier de configuration : seul le nonce (12 o.) + le
// texte chiffré + le tag d'authentification GCM (16 o.) sont écrits dans
// config.json, sous la forme "enc:" + base64(nonce || ciphertext).
//
// GCM garantit à la fois la confidentialité et l'intégrité : toute
// modification (ou un mauvais déchiffrement) est détectée.

const encryptedPrefix = "enc:"

// masterKeyFromEnv lit la clé maîtresse depuis l'environnement.
// Elle renvoie (clé, false, nil) si la variable est absente : le serveur
// fonctionne alors en mode non chiffré (compatibilité, clés en clair).
func masterKeyFromEnv() ([]byte, bool, error) {
	v := os.Getenv("P4RELAY_MASTER_KEY")
	if v == "" {
		return nil, false, nil
	}
	key, err := hex.DecodeString(v)
	if err != nil || len(key) != 32 {
		return nil, false, fmt.Errorf("P4RELAY_MASTER_KEY doit contenir exactement 64 caractères hexadécimaux (32 octets).")
	}
	return key, true, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// encryptValue chiffre une valeur avec AES-GCM. Valeur vide → renvoyée telle quelle.
func encryptValue(key []byte, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	aead, err := gcm(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(value), nil)
	return encryptedPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// isEncryptedValue dit si une valeur stockée est déjà au format chiffré.
func isEncryptedValue(value string) bool {
	return len(value) > len(encryptedPrefix) && value[:len(encryptedPrefix)] == encryptedPrefix
}

// decryptValue déchiffre une valeur au format "enc:..." ; toute autre valeur
// est renvoyée telle quelle (compatibilité avec les clés en clair).
func decryptValue(key []byte, value string) (string, error) {
	if !isEncryptedValue(value) {
		return value, nil
	}
	raw, err := base64.StdEncoding.DecodeString(value[len(encryptedPrefix):])
	if err != nil {
		return "", fmt.Errorf("clé API chiffrée illisible")
	}
	aead, err := gcm(key)
	if err != nil {
		return "", err
	}
	nonceSize := aead.NonceSize()
	if len(raw) <= nonceSize {
		return "", fmt.Errorf("clé API chiffrée corrompue")
	}
	plain, err := aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		return "", fmt.Errorf("déchiffrement de la clé API impossible (clé maîtresse incorrecte ou données modifiées) : %v", err)
	}
	return string(plain), nil
}
