package oauth

import "testing"

// RFC 7636 appendix B vector plus the failure modes the token endpoint
// relies on.
func TestVerifyPKCES256(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if !VerifyPKCES256(verifier, challenge) {
		t.Fatalf("RFC 7636 vector must verify")
	}
	bad := map[string][2]string{
		"wrong verifier":         {"dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXX", challenge},
		"plain (verifier==chal)": {verifier, verifier},
		"short verifier":         {"tooshort", challenge},
		"illegal char":           {verifier[:42] + "!", challenge},
		"empty":                  {"", challenge},
		"empty challenge":        {verifier, ""},
	}
	for name, c := range bad {
		if VerifyPKCES256(c[0], c[1]) {
			t.Errorf("%s: must not verify", name)
		}
	}
}

func TestValidPKCEString(t *testing.T) {
	ok := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~"
	if !ValidPKCEString(ok) {
		t.Errorf("full unreserved alphabet must be valid")
	}
	for _, s := range []string{"", "short", string(make([]byte, 129)), ok[:42], ok + "+", ok + "/", ok + "="} {
		if ValidPKCEString(s) {
			t.Errorf("%q must be invalid", s)
		}
	}
}
