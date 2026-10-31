package team

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"
)

// An upgrade from a meterm with cloud teams must leave no live credential
// behind: not the refresh token in the keychain, not a device key.
func TestForgetCloudRemovesSessionAndDeviceKeys(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	_ = keyring.Set(keyringService, "team-session:"+dirKey(dir), "refresh-secret")
	_ = keyring.Set(keyringService, "team-device:"+dirKey(dir)+":u1", "AGE-SECRET-KEY-1")
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(`{"uid":"u1","secret_in":"keyring"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "device-u1.json"), []byte(`{"id":"d","in":"keyring"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if !ForgetCloud(dir) {
		t.Fatal("found nothing to forget")
	}
	for _, f := range []string{"session.json", "device-u1.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s still there", f)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Errorf("state.json removed: %v", err)
	}
	for _, u := range []string{"team-session:" + dirKey(dir), "team-device:" + dirKey(dir) + ":u1"} {
		if _, err := keyring.Get(keyringService, u); err == nil {
			t.Errorf("keychain still holds %s", u)
		}
	}
	if ForgetCloud(dir) {
		t.Error("second run found something again")
	}
}
