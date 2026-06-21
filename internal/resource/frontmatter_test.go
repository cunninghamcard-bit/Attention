package resource

import "testing"

func TestParseFrontmatter(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		frontmatter map[string]string
		body        string
	}{
		{
			name:        "empty frontmatter block",
			content:     "---\n---\nbody",
			frontmatter: map[string]string{},
			body:        "body",
		},
		{
			name:        "empty frontmatter block only",
			content:     "---\n---",
			frontmatter: map[string]string{},
			body:        "",
		},
		{
			name:        "simple frontmatter",
			content:     "---\nname: demo\n---\nbody text",
			frontmatter: map[string]string{"name": "demo"},
			body:        "body text",
		},
		{
			name:        "no frontmatter",
			content:     "plain body",
			frontmatter: map[string]string{},
			body:        "plain body",
		},
		{
			name:        "unterminated frontmatter",
			content:     "---\nname: demo\nbody",
			frontmatter: map[string]string{},
			body:        "---\nname: demo\nbody",
		},
		{
			name:        "crlf newlines",
			content:     "---\r\nname: demo\r\n---\r\nbody",
			frontmatter: map[string]string{"name": "demo"},
			body:        "body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frontmatter, body, err := ParseFrontmatter(tt.content)
			if err != nil {
				t.Fatalf("ParseFrontmatter(%q) error: %v", tt.content, err)
			}
			if body != tt.body {
				t.Errorf("body = %q, want %q", body, tt.body)
			}
			if len(frontmatter) != len(tt.frontmatter) {
				t.Errorf("frontmatter = %v, want %v", frontmatter, tt.frontmatter)
			}
			for k, want := range tt.frontmatter {
				if got := frontmatter[k]; got != want {
					t.Errorf("frontmatter[%q] = %q, want %q", k, got, want)
				}
			}
		})
	}
}
