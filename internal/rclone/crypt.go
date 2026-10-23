// Package rclone reads and writes files in the format of rclone's crypt backend
// (https://rclone.org/crypt/#file-encryption). The operator's backup Jobs encrypt remote
// backups with a stock rclone; the operator itself decrypts the small files it needs (the
// manifest and the identity bundle of a backup) with this package, so it needs no rclone
// binary. File names are not encrypted (filename_encryption = off): rclone only appends
// ".bin" to them.
//
// Format: the key material is scrypt(password, salt, N=16384, r=8, p=1, 80 bytes), of which the
// first 32 bytes are the data key; without a second password (password2) the salt is rclone's
// built-in default. A file is the 8-byte magic "RCLONE\x00\x00", a random 24-byte nonce, and the
// plaintext in blocks of 64 KiB, each sealed with NaCl secretbox (XSalsa20-Poly1305) under the
// nonce, which is incremented (little endian) after every block.
package rclone

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/scrypt"
)

const (
	fileMagic     = "RCLONE\x00\x00"
	nonceSize     = 24
	headerSize    = len(fileMagic) + nonceSize
	blockDataSize = 64 * 1024
	blockSize     = blockDataSize + secretbox.Overhead

	// EncryptedSuffix is what rclone appends to the name of an encrypted file when file names
	// are not encrypted.
	EncryptedSuffix = ".bin"

	// MinKeyLength is the shortest key the operator accepts for backups.
	MinKeyLength = 16
	// MaxKeyLength bounds the key (it is passed through an environment variable).
	MaxKeyLength = 1024
)

// defaultSalt is rclone's salt when no password2 is configured (backend/crypt/cipher.go).
var defaultSalt = []byte{0xA8, 0x0D, 0xF4, 0x3A, 0x8F, 0xBD, 0x03, 0x08, 0xA7, 0xCA, 0xB8, 0x3E, 0x58, 0x1F, 0x86, 0xB1}

var (
	// ErrNotEncrypted: the data does not start with rclone's file header.
	ErrNotEncrypted = errors.New("not an rclone crypt file (bad header)")
	// ErrAuthentication: a block failed authentication: the key is wrong, or the file was
	// changed or truncated.
	ErrAuthentication = errors.New("decryption failed: wrong key, or the file is corrupt")
)

// Cipher encrypts and decrypts file contents with the keys derived from one password.
type Cipher struct {
	dataKey [32]byte
}

// cipherCache keeps derived keys per password digest: scrypt costs about 16 MiB and tens of
// milliseconds, and the operator reads several files with the same key.
var cipherCache sync.Map

// NewCipher derives the data key from password as rclone does without a password2.
func NewCipher(password string) (*Cipher, error) {
	if password == "" {
		return nil, errors.New("empty password")
	}
	sum := sha256.Sum256([]byte(password))
	if c, ok := cipherCache.Load(sum); ok {
		return c.(*Cipher), nil
	}
	key, err := scrypt.Key([]byte(password), defaultSalt, 16384, 8, 1, 32+32+16)
	if err != nil {
		return nil, err
	}
	c := &Cipher{}
	copy(c.dataKey[:], key)
	cipherCache.Store(sum, c)
	return c, nil
}

// Fingerprint identifies the key without revealing it or the password: a SHA-256 over the
// derived data key, so that guessing the password costs an scrypt per guess, as attacking the
// backups themselves does. 16 hex digits.
func (c *Cipher) Fingerprint() string {
	h := sha256.New()
	h.Write([]byte("forgejo-operator backup key fingerprint v1\x00"))
	h.Write(c.dataKey[:])
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Decrypt returns the plaintext of an encrypted file.
func (c *Cipher) Decrypt(data []byte) ([]byte, error) {
	if len(data) < headerSize || string(data[:len(fileMagic)]) != fileMagic {
		return nil, ErrNotEncrypted
	}
	var nonce [nonceSize]byte
	copy(nonce[:], data[len(fileMagic):headerSize])
	data = data[headerSize:]
	out := make([]byte, 0, max(0, len(data)-(len(data)/blockSize+1)*secretbox.Overhead))
	for len(data) > 0 {
		n := min(blockSize, len(data))
		if n <= secretbox.Overhead {
			return nil, ErrAuthentication // a truncated block
		}
		var ok bool
		out, ok = secretbox.Open(out, data[:n], &nonce, &c.dataKey)
		if !ok {
			return nil, ErrAuthentication
		}
		data = data[n:]
		increment(&nonce)
	}
	return out, nil
}

// Encrypt returns the encrypted file of plaintext, with a nonce read from random.
func (c *Cipher) Encrypt(plaintext []byte, random io.Reader) ([]byte, error) {
	var nonce [nonceSize]byte
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		return nil, fmt.Errorf("reading nonce: %w", err)
	}
	out := bytes.NewBuffer(make([]byte, 0, headerSize+len(plaintext)+(len(plaintext)/blockDataSize+1)*secretbox.Overhead))
	out.WriteString(fileMagic)
	out.Write(nonce[:])
	for len(plaintext) > 0 {
		n := min(blockDataSize, len(plaintext))
		out.Write(secretbox.Seal(nil, plaintext[:n], &nonce, &c.dataKey))
		plaintext = plaintext[n:]
		increment(&nonce)
	}
	return out.Bytes(), nil
}

// increment adds one to the little-endian nonce, as rclone does after each block.
func increment(n *[nonceSize]byte) {
	for i := range n {
		n[i]++
		if n[i] != 0 {
			return
		}
	}
}

// ValidateKey checks a backup key as the operator requires it: long enough, and without
// leading or trailing white space, which tools add easily and which would silently change the
// key (rclone uses the password as it is).
func ValidateKey(key string) error {
	switch {
	case strings.TrimSpace(key) != key:
		return errors.New("the key has leading or trailing white space (e.g. a final newline)")
	case len(key) < MinKeyLength:
		return fmt.Errorf("the key is shorter than %d characters", MinKeyLength)
	case len(key) > MaxKeyLength:
		return fmt.Errorf("the key is longer than %d characters", MaxKeyLength)
	case strings.ContainsAny(key, "\n\r\x00"):
		return errors.New("the key contains a line break or a NUL character")
	}
	return nil
}
