package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

func main() {
	envPath := ".env"
	versionFile := "go.mod" // or you can use a separate version.txt

	// --- Read current version ---
	appVersion := getCurrentVersion(envPath, versionFile)
	newVersion := bumpVersion(appVersion)

	// --- Update .env ---
	updateEnv(envPath, newVersion)

	// --- Update go.mod (replace a version placeholder if needed) ---
	updateGoMod(versionFile, newVersion)

	fmt.Printf("🔼 Bumped version to %s\n", newVersion)
}

func getCurrentVersion(envPath, versionFile string) string {
	// Try to get from .env first
	if f, err := os.Open(envPath); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "APP_VERSION=") {
				return strings.TrimPrefix(line, "APP_VERSION=")
			}
		}
	}

	// Fallback: get from go.mod (look for a placeholder like "// version X.Y.Z")
	if f, err := os.ReadFile(versionFile); err == nil {
		re := regexp.MustCompile(`(?m)^//\s*version\s*(\d+\.\d+\.\d+)`)
		if matches := re.FindStringSubmatch(string(f)); len(matches) == 2 {
			return matches[1]
		}
	}

	// Default
	return "0.0.0"
}

func bumpVersion(ver string) string {
	parts := strings.Split(ver, ".")
	if len(parts) != 3 {
		fmt.Println("Invalid version format, defaulting to 0.0.1")
		return "0.0.1"
	}
	patch, _ := strconv.Atoi(parts[2])
	patch++
	return fmt.Sprintf("%s.%s.%d", parts[0], parts[1], patch)
}

func updateEnv(envPath, newVersion string) {
	file, err := os.Open(envPath)
	if err != nil {
		fmt.Println("❌ Could not open .env:", err)
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var lines []string
	found := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "APP_VERSION=") {
			lines = append(lines, "APP_VERSION="+newVersion)
			found = true
		} else {
			lines = append(lines, line)
		}
	}
	if !found {
		lines = append(lines, "APP_VERSION="+newVersion)
	}

	os.WriteFile(envPath, []byte(strings.Join(lines, "\n")), 0644)
}

func updateGoMod(versionFile, newVersion string) {
	content, err := os.ReadFile(versionFile)
	if err != nil {
		fmt.Println("❌ Could not read go.mod:", err)
		return
	}

	re := regexp.MustCompile(`(?m)^(//\s*version\s*)(\d+\.\d+\.\d+)`)
	updated := re.ReplaceAllString(string(content), fmt.Sprintf("${1}%s", newVersion))

	os.WriteFile(versionFile, []byte(updated), 0644)
}
