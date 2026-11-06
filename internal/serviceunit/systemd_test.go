package serviceunit

import "testing"

func TestEnvironmentAssignmentQuotesTheWholeAssignment(t *testing.T) {
	for _, tc := range []struct{ name, value, want string }{
		{name: "plain path", value: "/etc/conveyor/.env", want: `"CONVEYOR_ENV_FILE=/etc/conveyor/.env"`},
		{name: "space", value: "/etc/my conveyor/.env", want: `"CONVEYOR_ENV_FILE=/etc/my conveyor/.env"`},
		{name: "percent sign", value: "/etc/100%h/.env", want: `"CONVEYOR_ENV_FILE=/etc/100%%h/.env"`},
		{name: "quote", value: `/etc/a"b/.env`, want: `"CONVEYOR_ENV_FILE=/etc/a\"b/.env"`},
		{name: "backslash", value: `/etc/a\b/.env`, want: `"CONVEYOR_ENV_FILE=/etc/a\\b/.env"`},
		{name: "dollar sign stays literal", value: "/etc/$HOME/.env", want: `"CONVEYOR_ENV_FILE=/etc/$HOME/.env"`},
		{name: "newline stays on the line", value: "/etc/a\nb/.env", want: `"CONVEYOR_ENV_FILE=/etc/a\nb/.env"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EnvironmentAssignment("CONVEYOR_ENV_FILE", tc.value); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}
