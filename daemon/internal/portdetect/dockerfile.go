package portdetect

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// parseDockerfile returns EXPOSE ports from the root Dockerfile. Weakest
// source: EXPOSE declares the container port, which equals the host port only
// under a -p N:N run — hence the explicit source label for the sheet.
func parseDockerfile(dir string) []Suggestion {
	f, err := os.Open(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Suggestion
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || !strings.EqualFold(fields[0], "EXPOSE") {
			continue
		}
		for _, field := range fields[1:] {
			if strings.HasSuffix(field, "/udp") {
				continue
			}
			port, _ := strconv.Atoi(strings.TrimSuffix(field, "/tcp"))
			if port > 0 {
				out = append(out, Suggestion{Name: "", Port: port, Source: "Dockerfile (EXPOSE)"})
			}
		}
	}
	return out
}
