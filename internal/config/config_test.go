package config

import "testing"

// requiredEnv sets the two credentials FromEnv refuses to run without, so a
// case can assert the field under test rather than a missing-key error.
func requiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LITELLM_API_KEY", "sk-test")
	t.Setenv("DOIT_API_KEY", "doit-test")
}

// The DEFAULT is what keeps an existing deployment byte-identical, so it is
// asserted here rather than inferred. The mapper tests cannot cover it: they
// build mapper.Options by hand and never read the environment.
func TestUserEmailSourceDefaultsToNone(t *testing.T) {
	requiredEnv(t)

	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}

	if c.UserEmailSource != UserEmailSourceNone {
		t.Errorf("UserEmailSource = %q with GENAI_USER_EMAIL_SOURCE unset, want %q",
			c.UserEmailSource, UserEmailSourceNone)
	}
}

func TestUserEmailSourceAccepted(t *testing.T) {
	for _, want := range []UserEmailSource{UserEmailSourceNone, UserEmailSourceEndUser, UserEmailSourceKeyAlias} {
		t.Run(string(want), func(t *testing.T) {
			requiredEnv(t)
			t.Setenv("GENAI_USER_EMAIL_SOURCE", string(want))

			c, err := FromEnv()
			if err != nil {
				t.Fatal(err)
			}

			if c.UserEmailSource != want {
				t.Errorf("UserEmailSource = %q, want %q", c.UserEmailSource, want)
			}
		})
	}
}

// A typo must stop the exporter at startup. Exporting nothing for a whole
// lookback window, and only then being noticed on a blank dashboard, is the
// failure this prevents.
func TestUserEmailSourceRejectsUnknown(t *testing.T) {
	for _, v := range []string{"email", "user_email", "END_USER", "true"} {
		t.Run(v, func(t *testing.T) {
			requiredEnv(t)
			t.Setenv("GENAI_USER_EMAIL_SOURCE", v)

			if _, err := FromEnv(); err == nil {
				t.Errorf("FromEnv accepted GENAI_USER_EMAIL_SOURCE=%q", v)
			}
		})
	}
}
