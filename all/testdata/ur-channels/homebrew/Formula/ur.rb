# ur's Homebrew formula for Linux (sidecar DESIGN.md §4.10, §4.18), rendered by
# build/all/ur-channels.zsh from a promoted release's signed SHA256SUMS. Linux only: on macOS ur is the
# cask's bundle, since a formula's bare binary would lose the bundle's profile, and with it the
# keychain (D-CORE-12). It installs the release's static binary and never builds from source.
class Ur < Formula
  desc "Command-line client and git remote helper for recURd"
  homepage "https://ur.io/recurd"
  version "2026.11.6-1234567"
  license "MPL-2.0"

  on_intel do
    url "https://github.com/urnetwork/build/releases/download/v2026.11.6-1234567/ur-2026.11.6-1234567-linux-amd64.tar.gz"
    sha256 "1d21f7c476470426e513ab301aed8dffcb52c09d2c7dbd6cb5b4352f9e054b43"
  end
  on_arm do
    url "https://github.com/urnetwork/build/releases/download/v2026.11.6-1234567/ur-2026.11.6-1234567-linux-arm64.tar.gz"
    sha256 "ddec8180437b1d8c7bc3927b204dd5f2e9e37befcbb1f91a3433d40626c96d5a"
  end

  depends_on :linux

  def install
    bin.install "ur"
    bin.install_symlink "ur" => "git-remote-ur"
  end

  test do
    assert_match "ur ", shell_output("#{bin}/ur --version")
  end
end
