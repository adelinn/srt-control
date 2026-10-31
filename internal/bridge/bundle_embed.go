//go:build bundleffmpeg

package bridge

import (
	"embed"
	"strings"
)

// The files in bundled/ are put there by CI (see .github/workflows/build.yml)
// and are not in git:
//
//	ffmpeg.gz      gzip-compressed ffmpeg binary for the target OS
//	ffmpeg.sha256  hex SHA-256 of the uncompressed binary
//	LICENSE.txt    ffmpeg's license
//	SOURCE.txt     which build it is and where its source code is
//
//go:embed bundled/ffmpeg.gz bundled/ffmpeg.sha256 bundled/LICENSE.txt bundled/SOURCE.txt
var bundled embed.FS

func init() {
	read := func(name string) []byte {
		b, err := bundled.ReadFile("bundled/" + name)
		if err != nil {
			panic(err) // embedded at build time, can't be missing
		}
		return b
	}
	source := read("SOURCE.txt")
	bundle.gz = read("ffmpeg.gz")
	bundle.sha256 = strings.TrimSpace(string(read("ffmpeg.sha256")))
	bundle.about = strings.TrimSpace(strings.SplitN(string(source), "\n", 2)[0])
	bundle.extras = map[string][]byte{"LICENSE.txt": read("LICENSE.txt"), "SOURCE.txt": source}
}
