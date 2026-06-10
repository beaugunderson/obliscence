package main

import "testing"

func TestCorrectionScore(t *testing.T) {
	// Genuine corrections should clear the default min-score (3) comfortably.
	corrections := []string{
		"no don't do that, just fix it",
		"do not ever fucking Any again. use the correct types",
		"stop making shit up and just go look at the PR",
		"that's not what i wanted at all",
		"why did you do that, I didn't ask",
		"i already told you not to ignore mypy errors",
		"revert everything, that's wrong",
	}
	for _, c := range corrections {
		if got := correctionScore(c); got < 3 {
			t.Errorf("correctionScore(%q) = %d, want >= 3", c, got)
		}
	}

	// Excitement / praise (even with profanity, caps, "!") is not a correction.
	notCorrections := []string{
		"wow that's actually a really fucking high quality result",
		"haha holy shit that is WILD. that is INSANE",
		"lol nice, that worked",
		"that's fucking awesome, love it",
		"can you add a test for the parser",
		"let's go with option 2",
		"",
	}
	for _, c := range notCorrections {
		if got := correctionScore(c); got >= 3 {
			t.Errorf("correctionScore(%q) = %d, want < 3", c, got)
		}
	}
}
