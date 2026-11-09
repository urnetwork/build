# ur's Homebrew cask for macOS (sidecar DESIGN.md §4.10, §4.18), rendered by build/all/ur-channels.zsh
# from a promoted release's signed SHA256SUMS. It installs the notarized ur.app as the release built
# it, never a bare binary, since ur's restricted entitlements need the bundle's embedded profile
# (D-CORE-12), and links ur and git-remote-ur to its executable in Homebrew's bin directory. That is
# not /usr/local/bin on Apple silicon, so harness configs take ur's own absolute path.
cask "ur" do
  arch arm: "arm64", intel: "amd64"

  version "2026.11.6-1234567"
  sha256 arm:   "bbdc8803697bccffefdfeff96786026b2d5a5c929fbd8bd9c0ffd986f7dcc34f",
         intel: "ad6508426da39d002c880fbba4abfc1915c2df22d03bf430bd5338ec44bdcf61"

  url "https://github.com/urnetwork/build/releases/download/v#{version}/ur-#{version}-darwin-#{arch}.tar.gz"
  name "ur"
  desc "Command-line client and git remote helper for recURd"
  homepage "https://ur.io/recurd"

  depends_on macos: ">= :monterey"

  binary "ur.app/Contents/MacOS/ur", target: "ur"
  binary "ur.app/Contents/MacOS/ur", target: "git-remote-ur"
end
