// Package e2ee seals file content on the client so the server stores bytes it
// cannot read. The key lives only in the share link's fragment, which browsers
// never send to a server.
//
// From one 32-byte key, HKDF-SHA256 derives three: one to encrypt, one to name
// content, and one to derive nonces. A blob's id is the HMAC-SHA256 of its
// plaintext, so identical files still share an id, while the server cannot
// check a guess at what a file contains. Content is sealed with AES-256-GCM
// using a nonce derived from the id, and the id is authenticated with it, so
// the server cannot present one file's ciphertext as another's.
//
// A sealed blob is: version, flags, then the ciphertext and tag. The plaintext
// is compressed with raw deflate first when that makes it smaller, which the
// flags say.
package e2ee

import (
	"bytes"
	"compress/flate"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

const (
	// Version is the sealed format.
	Version = 1
	// KeyBytes is the size of a key.
	KeyBytes = 32

	flagDeflate = 1
	nonceBytes  = 12
	minDeflate  = 64
)

var errMalformed = errors.New("e2ee: sealed content is malformed")

// Key is the secret a session's content is sealed with.
type Key [KeyBytes]byte

// NewKey returns a random key.
func NewKey() (Key, error) {
	var k Key
	_, err := rand.Read(k[:])
	return k, err
}

// ParseKey reads a key written by [Key.String].
func ParseKey(s string) (Key, error) {
	var k Key
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(raw) != KeyBytes {
		return k, errors.New("e2ee: the key is not valid")
	}
	copy(k[:], raw)
	return k, nil
}

// String is the key in the form a share link carries: unpadded base64url.
func (k Key) String() string { return base64.RawURLEncoding.EncodeToString(k[:]) }

// Cipher seals and opens content under one key.
type Cipher struct {
	aead     cipher.AEAD
	idKey    []byte
	nonceKey []byte
}

// New derives a cipher's keys from k.
func New(k Key) (*Cipher, error) {
	derive := func(label string) ([]byte, error) {
		return hkdf.Key(sha256.New, k[:], nil, "biga/e2ee/v1/"+label, KeyBytes)
	}
	enc, err := derive("enc")
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(enc)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	idKey, err := derive("id")
	if err != nil {
		return nil, err
	}
	nonceKey, err := derive("nonce")
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead, idKey: idKey, nonceKey: nonceKey}, nil
}

// ID names content: the hex HMAC-SHA256 of the plaintext, in the same shape as
// the SHA-256 hash it replaces.
func (c *Cipher) ID(plaintext []byte) string {
	mac := hmac.New(sha256.New, c.idKey)
	mac.Write(plaintext)
	return hex.EncodeToString(mac.Sum(nil))
}

func (c *Cipher) nonce(id string) []byte {
	mac := hmac.New(sha256.New, c.nonceKey)
	mac.Write([]byte(id))
	return mac.Sum(nil)[:nonceBytes]
}

func aad(header []byte, id string) []byte {
	return append(bytes.Clone(header), id...)
}

// Seal encrypts plaintext under the id [Cipher.ID] gave it. The same plaintext
// always seals to the same bytes, so the server can still store it once.
func (c *Cipher) Seal(id string, plaintext []byte) []byte {
	body, flags := plaintext, byte(0)
	if len(plaintext) >= minDeflate {
		if packed := deflate(plaintext); len(packed) < len(plaintext) {
			body, flags = packed, flagDeflate
		}
	}
	header := []byte{Version, flags}
	return c.aead.Seal(header, c.nonce(id), body, aad(header, id))
}

// Open decrypts what [Cipher.Seal] produced for id. It fails if the content
// was altered, or belongs to a different id or key.
func (c *Cipher) Open(id string, sealed []byte) ([]byte, error) {
	if len(sealed) < 2+c.aead.Overhead() || sealed[0] != Version || sealed[1]&^flagDeflate != 0 {
		return nil, errMalformed
	}
	body, err := c.aead.Open(nil, c.nonce(id), sealed[2:], aad(sealed[:2], id))
	if err != nil {
		return nil, fmt.Errorf("e2ee: cannot open content: %w", err)
	}
	if sealed[1]&flagDeflate == 0 {
		return body, nil
	}
	return io.ReadAll(flate.NewReader(bytes.NewReader(body)))
}

func deflate(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
	_, _ = w.Write(data)
	_ = w.Close()
	return buf.Bytes()
}
