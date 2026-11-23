class Icloudsync < Formula
  desc "Mirror iCloud Drive, Photos and Shared Albums to an external disk"
  homepage "https://github.com/serebro/icloudsync"
  version "0.1.0"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/serebro/icloudsync/releases/download/v0.1.0/icloudsync_v0.1.0_darwin_arm64.tar.gz"
      sha256 "5bb3f03cbc24706e7df0cf7361e9132b7370bbd41f34a59cae1eb8fa98542396"
    else
      url "https://github.com/serebro/icloudsync/releases/download/v0.1.0/icloudsync_v0.1.0_darwin_amd64.tar.gz"
      sha256 "85b9bae83572aea726b153ceea11ba1598bf2010762ea645bfbd3b097d00e9fa"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/serebro/icloudsync/releases/download/v0.1.0/icloudsync_v0.1.0_linux_arm64.tar.gz"
      sha256 "cc206093cf44d3997703fc60406c3847c152160d38de5b826a57586aac742b63"
    else
      url "https://github.com/serebro/icloudsync/releases/download/v0.1.0/icloudsync_v0.1.0_linux_amd64.tar.gz"
      sha256 "64e6785940bf774b85674ba55043a3bf65fa58b0e4f8c16fd0cd1b9a64557ed5"
    end
  end

  def install
    bin.install "icloudsync"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/icloudsync version")
  end
end
