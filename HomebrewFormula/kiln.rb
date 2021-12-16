# typed: false
# frozen_string_literal: true

# This file was generated by GoReleaser. DO NOT EDIT.
class Kiln < Formula
  desc ""
  homepage ""
  version "0.63.0"

  on_macos do
    if Hardware::CPU.intel?
      url "https://github.com/pivotal-cf/kiln/releases/download/0.63.0/kiln-darwin-0.63.0.tar.gz"
      sha256 "6f422c5857a7b42f0b7f4239c1eefa9863580c7e25084c3c83c6d218dbb5f9e6"

      def install
        bin.install "kiln"
      end
    end
  end

  on_linux do
    if Hardware::CPU.intel?
      url "https://github.com/pivotal-cf/kiln/releases/download/0.63.0/kiln-linux-0.63.0.tar.gz"
      sha256 "a3528f338f2b25b1dbfbbbef0e2044f71fe9fae88e95bc9fb69f92e698587822"

      def install
        bin.install "kiln"
      end
    end
  end

  test do
    system "#{bin}/kiln --version"
  end
end
