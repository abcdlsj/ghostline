package ghostline

import "runtime/debug"

const modulePath = "github.com/abcdlsj/ghostline"

// TagVersion returns the Ghostline module version embedded in the running
// binary. Development builds and local replacements intentionally report an
// empty value because they do not carry a release tag.
func TagVersion() string {
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	if buildInfo.Main.Path == modulePath && buildInfo.Main.Version != "" && buildInfo.Main.Version != "(devel)" {
		return buildInfo.Main.Version
	}
	for _, dependency := range buildInfo.Deps {
		if dependency.Path != modulePath {
			continue
		}
		if dependency.Replace != nil {
			if dependency.Replace.Version == "" || dependency.Replace.Version == "(devel)" {
				return ""
			}
			return dependency.Replace.Version
		}
		if dependency.Version == "(devel)" {
			return ""
		}
		return dependency.Version
	}
	return ""
}
