package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Chiffrement AES-GCM des clés API (champ apiKey) dans data/config.json.
//
// La clé maîtresse (32 octets, 64 caractères hexadécimaux) est résolue ainsi :
//   1. variable d'environnement P4RELAY_MASTER_KEY (priorité, clé externe) ;
//   2. sinon, fichier data/key (généré automatiquement à la première ouverture,
//      jamais écrit ailleurs) ;
//   3. sinon, une clé est générée et écrite dans data/key.
//
// Le fichier de clé est volontairement séparé de config.json : il n'est jamais
// modifié par les mutations de configuration ni renvoyé par l'interface.
// Seul le nonce (12 o.) + le texte chiffré + le tag GCM (16 o.) sont écrits
// dans config.json, sous la forme "enc:" + base64(nonce || ciphertext).
//
// Les valeurs vides ne sont jamais chiffrées : un fournisseur ajouté sans clé
// conserve "apiKey": "" en l'état.
//
// GCM garantit à la fois la confidentialité et l'intégrité : toute
// modification (ou un mauvais déchiffrement) est détectée.

const encryptedPrefix = "enc:"

const masterKeyFileName = "key"

// masterKeyLegacyFileName est l'ancien nom du fichier de clé maîtresse.
// S'il existe au démarrage, il est promu vers masterKeyFileName (migration).
const masterKeyLegacyFileName = "P4RELAY_MASTER_KEY"

// newMasterKey génère une clé maîtresse aléatoire de 32 octets (64 hex).
func newMasterKey() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, err
	}
	return b, nil
}

// masterKeyFromEnv lit la clé maîtresse depuis l'environnement.
// Elle renvoie (clé, false, nil) si la variable est absente.
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

// promoteLegacyKeyFile renomme l'ancien fichier P4RELAY_MASTER_KEY en key
// (migration transparente). S'il existe déjà un fichier key, l'ancien est
// ignoré : la clé courante prévaut.
//
// Le résultat est une erreur, pas un signal muet. Une migration qui échoue
// laisse sur le disque une clé avec laquelle la configuration chiffrée a été
// produite : la continuer pour générer une nouvelle clé rendrait toutes les
// clés API indéchiffrables, sans message explicite. L'appelant doit donc
// s'arrêter sur toute erreur renvoyée.
func promoteLegacyKeyFile(dataDir string) error {
	legacy := filepath.Join(dataDir, masterKeyLegacyFileName)
	current := filepath.Join(dataDir, masterKeyFileName)
	if _, err := os.Stat(legacy); err != nil {
		if os.IsNotExist(err) {
			// Absence normale : rien à migrer.
			return nil
		}
		// Une erreur d'accès ou d'E/S n'est pas une absence : on ne peut pas
		// conclure qu'il n'y a rien à migrer.
		return fmt.Errorf("impossible de vérifier la présence de %s : %v", masterKeyLegacyFileName, err)
	}
	if _, err := os.Stat(current); err == nil {
		// Les deux fichiers existent : la clé courante a la priorité, l'ancien
		// est laissé en place (son contenu n'est plus utilisé).
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("impossible de vérifier la présence de %s : %v", masterKeyFileName, err)
	}
	if err := os.Rename(legacy, current); err != nil {
		return fmt.Errorf("migration de l'ancien fichier de clé %s vers %s impossible : %v",
			masterKeyLegacyFileName, masterKeyFileName, err)
	}
	return nil
}

// loadMasterKey résout la clé maîtresse : variable d'environnement, sinon
// fichier data/key, sinon génération + écriture du fichier.
// La clé renvoyée est toujours non nulle : le chiffrement est actif par défaut.
func LoadMasterKey(dataDir string) ([]byte, error) {
	if key, ok, err := masterKeyFromEnv(); err != nil {
		return nil, err
	} else if ok {
		return key, nil
	}
	if err := promoteLegacyKeyFile(dataDir); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, masterKeyFileName)
	raw, err := os.ReadFile(path)
	if err == nil {
		key, derr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if derr != nil || len(key) != 32 {
			return nil, fmt.Errorf("le fichier %s est illisible : il doit contenir 64 caractères hexadécimaux. Supprimez-le pour générer une nouvelle clé (les clés API chiffrées devront être ressaisies).", masterKeyFileName)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("impossible de lire %s : %v", masterKeyFileName, err)
	}
	key, err := newMasterKey()
	if err != nil {
		return nil, fmt.Errorf("impossible de générer la clé de chiffrement : %v", err)
	}
	data := []byte(hex.EncodeToString(key) + "\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, fmt.Errorf("impossible d'écrire %s : %v", masterKeyFileName, err)
	}
	return key, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// encryptValue chiffre une valeur avec AES-GCM. Valeur vide → renvoyée telle quelle.
func EncryptValue(key []byte, value string) (string, error) {
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
func IsEncryptedValue(value string) bool {
	return len(value) > len(encryptedPrefix) && value[:len(encryptedPrefix)] == encryptedPrefix
}

// decryptValue déchiffre une valeur au format "enc:..." ; toute autre valeur
// est renvoyée telle quelle (compatibilité avec les clés en clair).
func DecryptValue(key []byte, value string) (string, error) {
	if !IsEncryptedValue(value) {
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
