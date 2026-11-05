package report

import "testing"

func TestValidateFormats(t *testing.T) {
	for _, s := range []string{"", "html", "pdf", "xlsx", "all", "html,pdf", "HTM,excel"} {
		if err := ValidateFormats(s); err != nil {
			t.Fatalf("ValidateFormats(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"bad", "html,bad", "html,,bad"} {
		if err := ValidateFormats(s); err == nil {
			t.Fatalf("ValidateFormats(%q) should fail", s)
		}
	}
}
