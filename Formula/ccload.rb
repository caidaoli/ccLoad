class Ccload < Formula
  desc "Multi-protocol AI API gateway"
  homepage "https://github.com/caidaoli/ccLoad"
  version "4.11.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/caidaoli/ccLoad/releases/download/v#{version}/ccload-darwin-arm64"
      sha256 "0b8d0e0613de702818bdbf1f4eec839d31eebcaea6a2891e0470295c7598c243"
    end
    on_intel do
      url "https://github.com/caidaoli/ccLoad/releases/download/v#{version}/ccload-darwin-amd64"
      sha256 "2be3422620b1f0d65b007750547f3e60e8889ea1e0326f3b81d9bcf3a82e350f"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/caidaoli/ccLoad/releases/download/v#{version}/ccload-linux-arm64"
      sha256 "0a727e7e62d2ef1ca1b28199e37b5ec81864a8eb3cf87d6722b80b3e7f74b229"
    end
    on_intel do
      url "https://github.com/caidaoli/ccLoad/releases/download/v#{version}/ccload-linux-amd64"
      sha256 "098836779b56cfb39029c9d2fef3bd3fb64b9c1d4ba18c262d89eddf51ff95f0"
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
