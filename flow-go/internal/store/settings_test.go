package store

import "testing"

// An unwritten key is unset, not zero and not an error. A fresh database has no
// opinion about which account to use, and the caller has to be able to tell that
// apart from a stored zero — one means "no choice recorded", the other means
// "the first account, deliberately".
func TestUnsetSettingIsAbsentNotZero(t *testing.T) {
	st := openTemp(t)

	value, found, err := st.Setting(SettingKeyAccountIndex)
	if err != nil {
		t.Fatalf("Setting on an unwritten key failed: %v", err)
	}
	if found {
		t.Errorf("found = true for an unwritten key (value %q)", value)
	}
	if value != "" {
		t.Errorf("value = %q, want empty for an unwritten key", value)
	}
}

func TestSetSettingRoundTripsAndReplaces(t *testing.T) {
	st := openTemp(t)

	if err := st.SetSetting(SettingKeyAccountIndex, "2"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}
	value, found, err := st.Setting(SettingKeyAccountIndex)
	if err != nil || !found {
		t.Fatalf("Setting after write: value=%q found=%v err=%v", value, found, err)
	}
	if value != "2" {
		t.Errorf("value = %q, want 2", value)
	}

	// The second write has to replace, not duplicate or fail: a switch back to
	// the first account is the case that a primary-key clash would break.
	if err := st.SetSetting(SettingKeyAccountIndex, "0"); err != nil {
		t.Fatalf("SetSetting on an existing key failed: %v", err)
	}
	value, _, err = st.Setting(SettingKeyAccountIndex)
	if err != nil {
		t.Fatalf("Setting after replace failed: %v", err)
	}
	if value != "0" {
		t.Errorf("value = %q, want 0 after replace", value)
	}

	// A stored zero must come back as found, so it cannot be mistaken for unset.
	if _, found, _ := st.Setting(SettingKeyAccountIndex); !found {
		t.Error("a stored zero reported as unset")
	}
}
