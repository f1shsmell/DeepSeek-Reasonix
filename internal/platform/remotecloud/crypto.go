package remotecloud

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/hkdf"
)

const protocolVersion = 1

type hello struct {
	Version      int            `json:"v"`
	Type         string         `json:"type"`
	PublicKey    string         `json:"publicKey"`
	Salt         string         `json:"salt"`
	Capabilities map[string]int `json:"capabilities,omitempty"`
}

type sealedMessage struct {
	Version    int    `json:"v"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
	// Bin marks a plaintext that is raw bytes, not JSON: the bulk lane's
	// parts. The envelope itself stays JSON text either way.
	Bin bool `json:"bin,omitempty"`
}

type sessionCipher struct {
	aead cipher.AEAD
	aad  []byte
	seen map[string]struct{}
	// sealMu guards seal: command goroutines and push pumps seal concurrently,
	// while open stays on the single read loop.
	sealMu sync.Mutex
}

func newSessionCipher(private *ecdh.PrivateKey, deviceID, payload string) (*sessionCipher, error) {
	var greeting hello
	if err := json.Unmarshal([]byte(payload), &greeting); err != nil {
		return nil, err
	}
	if greeting.Version != protocolVersion || greeting.Type != "hello" {
		return nil, errors.New("remote cloud: unsupported handshake")
	}
	peerBytes, err := base64.RawURLEncoding.DecodeString(greeting.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("remote cloud: controller public key: %w", err)
	}
	peer, err := ecdh.X25519().NewPublicKey(peerBytes)
	if err != nil {
		return nil, fmt.Errorf("remote cloud: controller public key: %w", err)
	}
	salt, err := base64.RawURLEncoding.DecodeString(greeting.Salt)
	if err != nil || len(salt) != 16 {
		return nil, errors.New("remote cloud: invalid handshake salt")
	}
	secret, err := private.ECDH(peer)
	if err != nil {
		return nil, fmt.Errorf("remote cloud: key agreement: %w", err)
	}
	info := "reasonix-remote-v1|" + deviceID
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, secret, salt, []byte(info)), key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sessionCipher{aead: aead, aad: []byte(info), seen: make(map[string]struct{})}, nil
}

func (s *sessionCipher) seal(value any) (string, error) {
	plain, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return s.sealPlain(plain, false)
}

// sealBytes seals raw bytes for the bulk lane: a file chunk crosses without
// being wrapped in JSON or base64 first, so only the envelope inflates it.
func (s *sessionCipher) sealBytes(plain []byte) (string, error) {
	return s.sealPlain(plain, true)
}

func (s *sessionCipher) sealPlain(plain []byte, bin bool) (string, error) {
	s.sealMu.Lock()
	defer s.sealMu.Unlock()
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	wire := sealedMessage{
		Version:    protocolVersion,
		Nonce:      base64.RawURLEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawURLEncoding.EncodeToString(s.aead.Seal(nil, nonce, plain, s.aad)),
		Bin:        bin,
	}
	encoded, err := json.Marshal(wire)
	return string(encoded), err
}

func (s *sessionCipher) open(payload string, out any) error {
	plain, bin, err := s.openPlain(payload)
	if err != nil {
		return err
	}
	if bin {
		return errors.New("remote cloud: binary message on the command lane")
	}
	return json.Unmarshal(plain, out)
}

// openBytes opens one bulk part. A JSON message offered as a part is refused:
// the lanes must not be interchangeable, or a command could pose as bytes.
func (s *sessionCipher) openBytes(payload string) ([]byte, error) {
	plain, bin, err := s.openPlain(payload)
	if err != nil {
		return nil, err
	}
	if !bin {
		return nil, errors.New("remote cloud: command message on the bulk lane")
	}
	return plain, nil
}

func (s *sessionCipher) openPlain(payload string) ([]byte, bool, error) {
	var wire sealedMessage
	if err := json.Unmarshal([]byte(payload), &wire); err != nil {
		return nil, false, err
	}
	if wire.Version != protocolVersion {
		return nil, false, errors.New("remote cloud: unsupported message version")
	}
	if _, duplicate := s.seen[wire.Nonce]; duplicate {
		return nil, false, errors.New("remote cloud: replayed message")
	}
	nonce, err := base64.RawURLEncoding.DecodeString(wire.Nonce)
	if err != nil || len(nonce) != s.aead.NonceSize() {
		return nil, false, errors.New("remote cloud: invalid message nonce")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(wire.Ciphertext)
	if err != nil {
		return nil, false, errors.New("remote cloud: invalid ciphertext")
	}
	plain, err := s.aead.Open(nil, nonce, ciphertext, s.aad)
	if err != nil {
		return nil, false, errors.New("remote cloud: message authentication failed")
	}
	if len(s.seen) >= 256 {
		clear(s.seen)
	}
	s.seen[wire.Nonce] = struct{}{}
	return plain, wire.Bin, nil
}
