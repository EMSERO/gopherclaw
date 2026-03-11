package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// FuzzVerifyHMAC fuzzes the HMAC verification function with random bodies,
// signatures, and secrets to ensure it never panics and only returns true
// when the signature is actually valid.
func FuzzVerifyHMAC(f *testing.F) {
	// Seed: valid HMAC pair
	secret := "test-secret"
	body := []byte(`{"message":"hello"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	validSig := hex.EncodeToString(mac.Sum(nil))

	f.Add(body, validSig, secret)
	f.Add(body, "sha256="+validSig, secret)
	f.Add([]byte(""), "", "")
	f.Add([]byte(""), "sha256=", "")
	f.Add([]byte("abc"), "deadbeef", "secret")
	f.Add([]byte("abc"), "sha256=deadbeef", "secret")
	f.Add(body, "not-hex-at-all!!!", secret)
	f.Add([]byte{0, 1, 2, 255}, "00", "\x00")
	f.Add(body, "sha256=", secret)
	f.Add(body, "sha256", secret)

	f.Fuzz(func(t *testing.T, body []byte, sig, secret string) {
		got := verifyHMAC(body, sig, secret)

		// Verify correctness: if the signature matches, verifyHMAC must return true.
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		expected := hex.EncodeToString(mac.Sum(nil))

		rawSig := sig
		if len(rawSig) > 7 && rawSig[:7] == "sha256=" {
			rawSig = rawSig[7:]
		}
		shouldMatch := hmac.Equal([]byte(expected), []byte(rawSig))

		if got != shouldMatch {
			t.Errorf("verifyHMAC(body=%q, sig=%q, secret=%q) = %v, want %v",
				body, sig, secret, got, shouldMatch)
		}
	})
}

// FuzzWebhookRequestParsing fuzzes JSON parsing of webhook request bodies
// to ensure json.Unmarshal into webhookRequest never panics.
func FuzzWebhookRequestParsing(f *testing.F) {
	f.Add([]byte(`{"message":"hello","stream":false}`))
	f.Add([]byte(`{"message":"","stream":true}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"message":123}`))
	f.Add([]byte(`{"message":"a","stream":"yes"}`))
	f.Add([]byte(`{"extra":"field","message":"hi"}`))
	f.Add([]byte(``))
	f.Add([]byte(`{`))
	f.Add([]byte(`{"message":"\u0000\u001f"}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var req webhookRequest
		// Must not panic. Errors are expected and fine.
		_ = json.Unmarshal(data, &req)
	})
}
