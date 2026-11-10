package e2ee

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func testKey(t *testing.T, seed byte) *Cipher {
	t.Helper()
	var k Key
	for i := range k {
		k[i] = seed + byte(i)
	}
	c, err := New(k)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sample() map[string][]byte {
	noise := make([]byte, 300)
	sum := sha256.Sum256([]byte("noise"))
	for i := range noise {
		sum = sha256.Sum256(sum[:])
		noise[i] = sum[0]
	}
	return map[string][]byte{
		"empty":          {},
		"short":          []byte("hello\n"),
		"unicode":        []byte("héllo ✓ wörld\n"),
		"not utf-8":      {0x61, 0xff, 0xfe, 0x62, 0x00, 0x63},
		"compressible":   bytes.Repeat([]byte("abc\n"), 1000),
		"incompressible": noise,
	}
}

func TestRoundTrip(t *testing.T) {
	c := testKey(t, 1)
	for name, plain := range sample() {
		id := c.ID(plain)
		sealed := c.Seal(id, plain)
		got, err := c.Open(id, sealed)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("%s: got %q, %v", name, got, err)
		}
	}
}

func TestIDsAreKeyedStableAndShapedLikeAHash(t *testing.T) {
	a, b := testKey(t, 1), testKey(t, 2)
	plain := []byte("same file\n")
	id := a.ID(plain)
	plainHash := sha256.Sum256(plain)
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Errorf("id = %q", id)
	}
	if id != a.ID(plain) || id == b.ID(plain) || id == hex.EncodeToString(plainHash[:]) {
		t.Error("an id is stable under one key, differs under another, and is not the plain SHA-256 a server could check a guess against")
	}
	if a.ID([]byte("other")) == id {
		t.Error("different content must have a different id")
	}
}

func TestSealingIsDeterministicSoIdenticalFilesDeduplicate(t *testing.T) {
	c := testKey(t, 1)
	plain := bytes.Repeat([]byte("line\n"), 500)
	id := c.ID(plain)
	if !bytes.Equal(c.Seal(id, plain), c.Seal(id, plain)) {
		t.Error("the same content sealed twice should be the same bytes")
	}
}

func TestSealedContentRevealsNothingReadable(t *testing.T) {
	c := testKey(t, 1)
	plain := []byte("password = hunter2-hunter2-hunter2\n")
	sealed := c.Seal(c.ID(plain), plain)
	if bytes.Contains(sealed, []byte("hunter2")) || bytes.Contains(sealed, []byte("password")) {
		t.Error("plaintext leaked into the sealed bytes")
	}
	big := bytes.Repeat([]byte("secret-token-value\n"), 200)
	if bytes.Contains(c.Seal(c.ID(big), big), []byte("secret-token")) {
		t.Error("compressed content leaked plaintext")
	}
}

func TestCompressionOnlyWhenItHelps(t *testing.T) {
	c := testKey(t, 1)
	noise := make([]byte, 400)
	_, _ = rand.Read(noise)
	if flags := c.Seal(c.ID(noise), noise)[1]; flags != 0 {
		t.Errorf("incompressible data was marked as deflated: flags %d", flags)
	}
	repetitive := bytes.Repeat([]byte("a"), 4000)
	sealed := c.Seal(c.ID(repetitive), repetitive)
	if sealed[1] != flagDeflate || len(sealed) > 200 {
		t.Errorf("repetitive data should be deflated: flags %d, %d bytes", sealed[1], len(sealed))
	}
}

func TestOpenRejectsAnythingThatWasChanged(t *testing.T) {
	c, other := testKey(t, 1), testKey(t, 2)
	plain := []byte("some file content that is long enough to matter, repeated. ")
	id := c.ID(plain)
	sealed := c.Seal(id, plain)

	flip := bytes.Clone(sealed)
	flip[len(flip)/2] ^= 0x01
	version := bytes.Clone(sealed)
	version[0] = 2
	flags := bytes.Clone(sealed)
	flags[1] |= 0x80
	deflated := bytes.Clone(sealed)
	deflated[1] ^= flagDeflate

	for name, tc := range map[string]struct {
		c      *Cipher
		id     string
		sealed []byte
	}{
		"a changed byte":                 {c, id, flip},
		"another file's id":              {c, c.ID([]byte("other")), sealed},
		"another key":                    {other, id, sealed},
		"truncated":                      {c, id, sealed[:10]},
		"empty":                          {c, id, nil},
		"an unknown version":             {c, id, version},
		"unknown flags":                  {c, id, flags},
		"flags changed to claim deflate": {c, id, deflated},
	} {
		if got, err := tc.c.Open(tc.id, tc.sealed); err == nil {
			t.Errorf("%s was accepted: %q", name, got)
		}
	}
}

func TestKeysRoundTripAsLinkFragments(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	s := k.String()
	if len(s) != 43 || strings.ContainsAny(s, "+/=") {
		t.Errorf("key text = %q", s)
	}
	back, err := ParseKey(s)
	if err != nil || back != k {
		t.Errorf("ParseKey = %v, %v", back, err)
	}
	for _, bad := range []string{"", "short", s + "A", strings.Repeat("!", 43), "AAAA"} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
	other, _ := NewKey()
	if other == k {
		t.Error("two new keys were equal")
	}
}

type vectorFile struct {
	Key   string `json:"key"`
	Cases []struct {
		Name      string `json:"name"`
		Plaintext string `json:"plaintext"`
		ID        string `json:"id"`
		Sealed    string `json:"sealed"`
	} `json:"cases"`
}

const vectorPath = "testdata/vectors.json"

func TestWriteVectors(t *testing.T) {
	if os.Getenv("BIGA_UPDATE_VECTORS") == "" {
		t.Skip("set BIGA_UPDATE_VECTORS=1 to regenerate the shared test vectors")
	}
	var k Key
	for i := range k {
		k[i] = 1 + byte(i)
	}
	c := testKey(t, 1)
	out := vectorFile{Key: k.String()}
	for _, name := range []string{"empty", "short", "unicode", "not utf-8", "compressible", "incompressible"} {
		plain := sample()[name]
		id := c.ID(plain)
		out.Cases = append(out.Cases, struct {
			Name      string `json:"name"`
			Plaintext string `json:"plaintext"`
			ID        string `json:"id"`
			Sealed    string `json:"sealed"`
		}{name, hex.EncodeToString(plain), id, hex.EncodeToString(c.Seal(id, plain))})
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vectorPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSharedVectorsStillOpen(t *testing.T) {
	data, err := os.ReadFile(vectorPath)
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	k, err := ParseKey(v.Key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(k)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) < 6 {
		t.Fatalf("only %d vectors", len(v.Cases))
	}
	for _, tc := range v.Cases {
		plain, _ := hex.DecodeString(tc.Plaintext)
		sealed, _ := hex.DecodeString(tc.Sealed)
		if c.ID(plain) != tc.ID {
			t.Errorf("%s: id changed", tc.Name)
		}
		got, err := c.Open(tc.ID, sealed)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("%s: opened %q, %v", tc.Name, got, err)
		}
	}
}
