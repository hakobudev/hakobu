package ops

import (
	"regexp"
	"strconv"
	"strings"
)

// Names hakobu picks so none has to be invented: a database or storage
// left unnamed is "<project>-<what it is>", and an R2 bucket adds a random
// part (bucketName).

// suggestName is "<project>-<kind>", or "-2", "-3"... after it when taken,
// with the project's part cut short to fit valid; "" if nothing fits.
func suggestName(project, kind string, valid *regexp.Regexp, taken func(string) bool) string {
	for i := 1; i < 100; i++ {
		tail := "-" + kind
		if i > 1 {
			tail += "-" + strconv.Itoa(i)
		}
		base := project
		if room := 40 - len(tail); len(base) > room {
			base = strings.TrimRight(base[:room], "-")
		}
		if name := base + tail; valid.MatchString(name) && !taken(name) {
			return name
		}
	}
	return ""
}

// bucketName is "<project>-<storage>-<suffix>", the storage's name alone
// when it already starts with the project's, cut short to R2's 63
// characters.
func bucketName(project, storage, suffix string) string {
	base := storage
	if !strings.HasPrefix(storage, project+"-") {
		base = project + "-" + storage
	}
	if room := 63 - len(suffix) - 1; len(base) > room {
		base = strings.TrimRight(base[:room], "-")
	}
	return base + "-" + suffix
}
