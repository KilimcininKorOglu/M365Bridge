package setup

import "testing"

func TestRefreshTokenFrom(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"success body", `{"token_type":"Bearer","refresh_token":"abc.def"}`, "abc.def"},
		{"failure body", `{"error":"invalid_grant","error_description":"expired"}`, ""},
		{"not json", `<html>Something went wrong</html>`, ""},
		{"empty", ``, ""},
	}
	for _, tc := range tests {
		if got := refreshTokenFrom([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: refreshTokenFrom = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestOnAppSurface(t *testing.T) {
	tests := []struct {
		name string
		loc  string
		want bool
	}{
		{"chat page", "https://m365.cloud.microsoft/chat", true},
		{"route page", "https://m365.cloud.microsoft/library/visuals/all", true},
		{"login host", "https://login.microsoftonline.com/common/oauth2/v2.0/authorize?x=1", false},
		{"landing hop", "https://m365.cloud.microsoft/landingv2", false},
		{"spalanding", "https://m365.cloud.microsoft/spalanding?code=abc", false},
		{"not a url", "::", false},
	}
	for _, tc := range tests {
		if got := onAppSurface(tc.loc); got != tc.want {
			t.Errorf("%s: onAppSurface(%q) = %v, want %v", tc.name, tc.loc, got, tc.want)
		}
	}
}
