# This file was generated by GoReleaser. DO NOT EDIT.
class Kiln < Formula
  desc ""
  homepage ""
  version "0.48.0"
  bottle :unneeded

  if OS.mac?
    url "https://github.com/pivotal-cf/kiln/releases/download/0.48.0/kiln-darwin-0.48.0.tar.gz"
    sha256 "8083dde83564de55d301a34e213a158ee3c87737dfbf05c6147ed99e23406c79"
  elsif OS.linux?
    if Hardware::CPU.intel?
      url "https://github.com/pivotal-cf/kiln/releases/download/0.48.0/kiln-linux-0.48.0.tar.gz"
      sha256 "3445ecb1f6c5baa90f11eeb83489f2f96759fe77004c2b41b50fd5eb168ffeb9"
    end
  end

  def install
    bin.install "kiln"
  end

  test do
    system "#{bin}/kiln --version"
  end
end
