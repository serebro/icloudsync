#!/bin/sh
# Writes Formula/icloudsync.rb for a release.
# Usage: scripts/gen-formula.sh vX.Y.Z path/to/SHA256SUMS [owner/repo]
set -eu
tag="$1"; sums="$2"; repo="${3:-serebro/icloudsync}"
ver="${tag#v}"

sum() {
  s="$(awk -v f="icloudsync_${tag}_$1_$2.tar.gz" '$2 == f {print $1}' "$sums")"
  [ -n "$s" ] || { echo "no checksum for $1/$2 in $sums" >&2; exit 1; }
  printf '%s' "$s"
}
url() { printf 'https://github.com/%s/releases/download/%s/icloudsync_%s_%s_%s.tar.gz' "$repo" "$tag" "$tag" "$1" "$2"; }

cat <<RUBY
class Icloudsync < Formula
  desc "Mirror iCloud Drive, Photos and Shared Albums to an external disk"
  homepage "https://github.com/${repo}"
  version "${ver}"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "$(url darwin arm64)"
      sha256 "$(sum darwin arm64)"
    else
      url "$(url darwin amd64)"
      sha256 "$(sum darwin amd64)"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "$(url linux arm64)"
      sha256 "$(sum linux arm64)"
    else
      url "$(url linux amd64)"
      sha256 "$(sum linux amd64)"
    end
  end

  def install
    bin.install "icloudsync"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/icloudsync version")
  end
end
RUBY
