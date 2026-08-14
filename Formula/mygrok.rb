# Homebrew formula for the mygrok client.
#
# The canonical copy lives in schappim/homebrew-mygrok; this one is here so
# the formula is versioned alongside the code it builds. After tagging a
# release, copy it into the tap with the new url + sha256:
#
#   brew install schappim/mygrok/mygrok
#
# Only the client is packaged. The server is Linux-only and installs via
# deploy/install-server.sh.
class Mygrok < Formula
  desc "Self-hosted tunnel client: forward a local port to a public HTTPS URL"
  homepage "https://github.com/schappim/mygrok"
  url "https://github.com/schappim/mygrok/archive/refs/tags/v1.0.1.tar.gz"
  sha256 "53dbebd7cf7af8be96d855b9a73feab1f488bed575ad0155d034c19c055368fd"
  license "MIT"
  head "https://github.com/schappim/mygrok.git", branch: "main"

  depends_on "go" => :build

  def install
    ldflags = "-s -w -X github.com/schappim/mygrok/internal/buildinfo.Version=v#{version}"
    system "go", "build", *std_go_args(ldflags: ldflags), "./cmd/mygrok"
  end

  def caveats
    <<~EOS
      mygrok needs a server to connect to. A Homebrew build deliberately
      ships without a default — point it at yours:

        echo 'server = "tunnel.example.com:7000"' >> ~/.mygrok/config.toml
        echo '<your-auth-token>' > ~/.mygrok/authtoken
        chmod 600 ~/.mygrok/authtoken

      Don't have a server yet? One command on any $5 VPS:
        https://github.com/schappim/mygrok/blob/main/docs/server.md
    EOS
  end

  test do
    assert_match "mygrok", shell_output("#{bin}/mygrok version")

    # Usage goes to stderr and exits 2, so fold it into stdout.
    help = shell_output("#{bin}/mygrok --help 2>&1", 2)
    assert_match "mygrok http", help
    assert_match "--subdomain", help

    # With no server configured anywhere, the client must say so plainly
    # rather than dialling something arbitrary. HOME is redirected so a
    # real ~/.mygrok/config.toml on the build machine can't mask this.
    ENV["HOME"] = testpath
    ENV.delete("MYGROK_SERVER")
    out = shell_output("#{bin}/mygrok http 3000 --subdomain=test --auth=x 2>&1", 1)
    assert_match "no tunnel server configured", out
  end
end
