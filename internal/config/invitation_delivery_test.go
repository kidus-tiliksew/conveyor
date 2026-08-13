package config

import "testing"

func TestInvitationDeliveryIsProcessOnly(t *testing.T) {
	t.Setenv(SMTPHostEnv, "smtp.example")
	t.Setenv(SMTPPortEnv, "")
	t.Setenv(SMTPUsernameEnv, "mailer")
	t.Setenv(SMTPPasswordEnv, "super-secret")
	t.Setenv(SMTPFromEnv, "Conveyor <conveyor@example.test>")
	t.Setenv(PublicURLEnv, "https://conveyor.example/")
	got := InvitationDeliveryFromEnvironment()
	if !got.SMTPConfigured() || got.Port != "587" || got.Password != "super-secret" || got.PublicURL != "https://conveyor.example" {
		t.Fatalf("delivery=%+v", got)
	}
	if _, err := MarshalWorkspaceDocument(&Config{}); err != nil {
		t.Fatal(err)
	}
}
