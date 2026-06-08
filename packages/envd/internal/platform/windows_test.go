//go:build windows

package platform

import "testing"

func TestRootSubpath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "empty",
			path: "",
			want: `C:\`,
		},
		{
			name: "root",
			path: "/",
			want: `C:\`,
		},
		{
			name: "slash child",
			path: "/tmp/data",
			want: `C:\tmp\data`,
		},
		{
			name: "backslash child",
			path: `\tmp\data`,
			want: `C:\tmp\data`,
		},
		{
			name: "relative child",
			path: `tmp\data`,
			want: `C:\tmp\data`,
		},
		{
			name: "absolute windows path",
			path: `D:\envd`,
			want: `D:\envd`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := rootSubpath(`C:\`, tt.path); got != tt.want {
				t.Fatalf("rootSubpath() = %q, want %q", got, tt.want)
			}
		})
	}
}
