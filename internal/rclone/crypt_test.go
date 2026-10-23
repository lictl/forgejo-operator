package rclone

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// testPassword encrypted the files in testdata, written by rclone v1.75.2 (crypt remote with
// filename_encryption = off and no password2):
//
//	RCLONE_CONFIG_ENC_PASSWORD=$(printf %s "$testPassword" | rclone obscure -) rclone copy plain enc:
const testPassword = "Zm9yZ2Vqby1vcGVyYXRvci10ZXN0LWtleS0xMjM0NTY3OA=="

// pattern is the plaintext of two.bin: bytes i % 251, one byte more than a block and nine more.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func TestDecryptRcloneFiles(t *testing.T) {
	c, err := NewCipher(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string][]byte{
		"empty.bin":     {},
		"hello.txt.bin": []byte("hello\n"),
		"two.bin.bin":   pattern(65546), // two blocks: the nonce is incremented between them
	} {
		data, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.Decrypt(data)
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: decrypted %d bytes that differ from the %d bytes rclone encrypted", file, len(got), len(want))
		}
	}
}

func TestDecryptRejectsWrongKeyAndDamage(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "two.bin.bin"))
	if err != nil {
		t.Fatal(err)
	}
	wrong, _ := NewCipher(testPassword + "x")
	if _, err := wrong.Decrypt(data); !errors.Is(err, ErrAuthentication) {
		t.Errorf("wrong key: got %v, want ErrAuthentication", err)
	}
	c, _ := NewCipher(testPassword)
	flipped := bytes.Clone(data)
	flipped[len(flipped)-1] ^= 1
	if _, err := c.Decrypt(flipped); !errors.Is(err, ErrAuthentication) {
		t.Errorf("changed byte: got %v, want ErrAuthentication", err)
	}
	if _, err := c.Decrypt(data[:len(data)-10]); !errors.Is(err, ErrAuthentication) {
		t.Errorf("truncated file: got %v, want ErrAuthentication", err)
	}
	if _, err := c.Decrypt(data[:headerSize+blockSize+5]); !errors.Is(err, ErrAuthentication) {
		t.Errorf("truncated last block: got %v, want ErrAuthentication", err)
	}
	if _, err := c.Decrypt([]byte(`{"plain":"json"}`)); !errors.Is(err, ErrNotEncrypted) {
		t.Errorf("plaintext: got %v, want ErrNotEncrypted", err)
	}
}

func TestEncryptRoundTripsAndMatchesRcloneSizes(t *testing.T) {
	c, _ := NewCipher(testPassword)
	for _, n := range []int{0, 1, 6, blockDataSize - 1, blockDataSize, blockDataSize + 1, 3*blockDataSize + 7} {
		plain := pattern(n)
		enc, err := c.Encrypt(plain, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		blocks := (n + blockDataSize - 1) / blockDataSize
		if want := headerSize + n + blocks*16; len(enc) != want {
			t.Errorf("%d bytes: encrypted size %d, rclone's is %d", n, len(enc), want)
		}
		got, err := c.Decrypt(enc)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("%d bytes: round trip failed (%v)", n, err)
		}
	}
}

func TestFingerprintDependsOnKeyOnly(t *testing.T) {
	a, _ := NewCipher(testPassword)
	b, _ := NewCipher(testPassword)
	other, _ := NewCipher(testPassword + "!")
	if a.Fingerprint() != b.Fingerprint() || len(a.Fingerprint()) != 16 {
		t.Errorf("fingerprint %q is not stable", a.Fingerprint())
	}
	if a.Fingerprint() == other.Fingerprint() {
		t.Error("different keys share a fingerprint")
	}
}

func TestValidateKey(t *testing.T) {
	for key, ok := range map[string]bool{
		testPassword:               true,
		"short":                    false,
		testPassword + "\n":        false,
		" " + testPassword:         false,
		"0123456789abcdef":         true,
		"0123456789\nabcdef":       false,
		string(make([]byte, 2000)): false,
	} {
		if err := ValidateKey(key); (err == nil) != ok {
			t.Errorf("ValidateKey(%q) = %v, want ok=%t", key, err, ok)
		}
	}
}
