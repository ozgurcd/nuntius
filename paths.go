package nuntius

import (
	"errors"
	"os"
	"path/filepath"
)

type resolvedPaths struct {
	Config, Data             string
	ConfigSource, DataSource string
	DefaultData              string
}

func notificationsSupported(goos string) bool { return goos == "darwin" }

// All environment and OS inputs are explicit so path rules do not depend on
// the test host. Empty environment variables count as unset.
func resolvePaths(goos, home string, env func(string) string, config, data string, configFlag, dataFlag bool) (resolvedPaths, error) {
	p := resolvedPaths{ConfigSource: "default", DataSource: "default"}
	configHome := env("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	p.Config = filepath.Join(configHome, "nuntius", "lanes.json")
	switch goos {
	case "darwin":
		p.DefaultData = filepath.Join(home, "Library", "Application Support", "nuntius")
	case "linux":
		dataHome := env("XDG_DATA_HOME")
		if dataHome == "" {
			dataHome = filepath.Join(home, ".local", "share")
		}
		p.DefaultData = filepath.Join(dataHome, "nuntius")
	}
	p.Data = p.DefaultData
	if v := env("NUNTIUS_CONFIG"); v != "" {
		p.Config, p.ConfigSource = v, "env"
	}
	if v := env("NUNTIUS_DATA_DIR"); v != "" {
		p.Data, p.DataSource = v, "env"
	}
	if configFlag {
		p.Config, p.ConfigSource = config, "flag"
	}
	if dataFlag {
		p.Data, p.DataSource = data, "flag"
	}
	if p.Data == "" {
		return p, errors.New("no default data directory on this OS; set NUNTIUS_DATA_DIR or --store DIRECTORY")
	}
	if !filepath.IsAbs(p.Data) {
		return p, errors.New("data directory must be absolute")
	}
	if p.Config == "" {
		return p, errors.New("configuration file must not be empty")
	}
	return p, nil
}

// This is only a presence probe. It never opens run contents or moves data.
func emptyStoreWarning(data, defaultData string) bool {
	if defaultData == "" || filepath.Clean(data) == filepath.Clean(defaultData) {
		return false
	}
	hasRuns := func(dir string) bool {
		st, err := os.Lstat(filepath.Join(dir, "runs"))
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return false
		}
		f, err := os.Open(filepath.Join(dir, "runs"))
		if err != nil {
			return false
		}
		defer f.Close()
		for {
			entries, err := f.ReadDir(100)
			for _, entry := range entries {
				if entry.IsDir() && runPattern.MatchString(entry.Name()) {
					return true
				}
			}
			if err != nil {
				return false
			}
		}
	}
	return !hasRuns(data) && hasRuns(defaultData)
}
