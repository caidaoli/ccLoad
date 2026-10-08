class Ccload < Formula
  desc "Multi-protocol AI API gateway"
  homepage "https://github.com/caidaoli/ccLoad"
  version "4.11.1"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/caidaoli/ccLoad/releases/download/v#{version}/ccload-darwin-arm64"
      sha256 "1ccb29bab5fdc473a98ce2c51c70952f608814fceedf93a7967d1dad30e2d277"
    end
    on_intel do
      url "https://github.com/caidaoli/ccLoad/releases/download/v#{version}/ccload-darwin-amd64"
      sha256 "1d2f01f93052345ca9ff14de30fc7874b9f3eaa47ad53280df815f01227579fd"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/caidaoli/ccLoad/releases/download/v#{version}/ccload-linux-arm64"
      sha256 "0a82e1ecf58aa128937b85fe82d885e2db0d791c6444c3ef38eef59bed07e81c"
    end
    on_intel do
      url "https://github.com/caidaoli/ccLoad/releases/download/v#{version}/ccload-linux-amd64"
      sha256 "e4eeab495e42af3f77f52be9cdc2bd13fb7077724760f6a11921fb40c7d1b0a1"
    end
  end

  def install
    libexec.install Dir["ccload-*"].first => "ccload"
    chmod 0755, libexec/"ccload"
    # Existing releases use this switch to disable in-process binary updates.
    (bin/"ccload").write_env_script libexec/"ccload", CCLOAD_CONTAINER: "1"
  end

  def caveats
    <<~EOS
      Before starting, run: mkdir -p #{var}/ccload
      Then create #{var}/ccload/.env with:
        CCLOAD_PASS=your_strong_password
      Protect it with: chmod 600 #{var}/ccload/.env

      Start with: brew services start caidaoli/ccload/ccload
      Open http://localhost:8080/web/
      Data and configuration: #{var}/ccload
      Logs: #{var}/log/ccload

      In-app updates are disabled; upgrade using brew upgrade.
    EOS
  end

  service do
    run [opt_bin/"ccload"]
    working_dir var/"ccload"
    log_path var/"log/ccload/output.log"
    error_log_path var/"log/ccload/error.log"
  end

  test do
    require "net/http"
    require "json"

    port = free_port
    pid = spawn({ "CCLOAD_PASS" => "homebrew-test-password", "PORT" => port.to_s,
                  "SQLITE_PATH" => (testpath/"ccload.db").to_s },
                (bin/"ccload").to_s, chdir: testpath.to_s,
                out: (testpath/"output.log").to_s, err: [:child, :out])
    begin
      response = nil
      60.times do
        sleep 1
        begin
          response = Net::HTTP.get_response(URI("http://127.0.0.1:#{port}/health"))
          break if response.is_a?(Net::HTTPSuccess)
        rescue Errno::ECONNREFUSED, Errno::ECONNRESET
          next
        end
      end
      assert_equal "200", response&.code, (testpath/"output.log").read
      assert_equal "ok", JSON.parse(response.body).dig("data", "status")
    ensure
      begin
        Process.kill("TERM", pid)
      rescue Errno::ESRCH
        # Preserve the health-check failure if startup exited early.
        nil
      end
      Process.wait(pid)
    end
  end
end
