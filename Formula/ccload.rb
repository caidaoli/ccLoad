class Ccload < Formula
  desc "Multi-protocol AI API gateway"
  homepage "https://github.com/caidaoli/ccLoad"
  version "5.0.1"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/caidaoli/ccLoad/releases/download/v#{version}/ccload-darwin-arm64"
      sha256 "ac32536cea48e181523ea09fbffa7899a00d67c1211b53380343c8147447aee2"
    end
    on_intel do
      url "https://github.com/caidaoli/ccLoad/releases/download/v#{version}/ccload-darwin-amd64"
      sha256 "fc42f3c11477184f524b01166b0e9c249efc81230469d07af70073c78caa7496"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/caidaoli/ccLoad/releases/download/v#{version}/ccload-linux-arm64"
      sha256 "d05db74d290117d0630ffef3da8b904d2928962fc0636b0504f80edbc601e193"
    end
    on_intel do
      url "https://github.com/caidaoli/ccLoad/releases/download/v#{version}/ccload-linux-amd64"
      sha256 "0dd55b6b33ba3a38a2745d78d7e3f02558694f399c93dbe995e9b6464af9a785"
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
