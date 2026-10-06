package config

import "testing"

func TestHealthURL(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		httpOnly bool
		want     string
		wantErr  bool
	}{
		{name: "autocert mode probes port 80", env: map[string]string{}, want: "http://127.0.0.1:80/healthz"},
		{name: "autocert ignores HTTP_ADDR", env: map[string]string{"HTTP_ADDR": ":9000"}, want: "http://127.0.0.1:80/healthz"},
		{name: "http-only default address", env: map[string]string{"HTTP_ONLY": "1"}, want: "http://127.0.0.1:8080/healthz"},
		{name: "http-only flag", env: map[string]string{"HTTP_ADDR": ":9000"}, httpOnly: true, want: "http://127.0.0.1:9000/healthz"},
		{name: "IPv4 wildcard", env: map[string]string{"HTTP_ONLY": "true", "HTTP_ADDR": "0.0.0.0:8081"}, want: "http://127.0.0.1:8081/healthz"},
		{name: "IPv6 wildcard", env: map[string]string{"HTTP_ONLY": "yes", "HTTP_ADDR": "[::]:8082"}, want: "http://127.0.0.1:8082/healthz"},
		{name: "specific address", env: map[string]string{"HTTP_ONLY": "1", "HTTP_ADDR": "10.0.0.5:8080"}, want: "http://10.0.0.5:8080/healthz"},
		{name: "specific IPv6 address", env: map[string]string{"HTTP_ONLY": "1", "HTTP_ADDR": "[::1]:8080"}, want: "http://[::1]:8080/healthz"},
		{name: "host name", env: map[string]string{"HTTP_ONLY": "1", "HTTP_ADDR": "localhost:8080"}, want: "http://localhost:8080/healthz"},
		{name: "bad HTTP_ONLY", env: map[string]string{"HTTP_ONLY": "maybe"}, wantErr: true},
		{name: "address without port", env: map[string]string{"HTTP_ONLY": "1", "HTTP_ADDR": "8080"}, wantErr: true},
		{name: "empty port", env: map[string]string{"HTTP_ONLY": "1", "HTTP_ADDR": "127.0.0.1:"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HealthURL(envMap(tt.env), tt.httpOnly)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("HealthURL = %q, want an error", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("HealthURL = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
