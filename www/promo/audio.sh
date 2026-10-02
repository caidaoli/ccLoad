#!/usr/bin/env bash
# 给宣传视频配旁白和背景音乐：edge-tts（微软 Edge 在线语音，免费、无需 Key）+ ffmpeg。
#
#   www/promo/audio.sh zh    # → www/assets/video/ccload-promo.zh-CN.mp4
#   www/promo/audio.sh en    # → www/assets/video/ccload-promo.en.mp4
#
# 画面流原样复制，只替换音轨；每句旁白的起始秒数对应 index.html 里的场景时间。
# 设置了 HTTPS_PROXY 时 edge-tts 走该代理。
set -euo pipefail

lang="${1:?usage: audio.sh zh|en}"

# 背景音乐：Mixkit “Close Up”（Michael Ramir C.），Mixkit Stock Music Free License：
# 可用于商业视频、无需署名，但不得再分发原曲文件——所以每次运行时下载，不入库。
# https://mixkit.co/free-stock-music/tag/technology/
music_url="https://assets.mixkit.co/music/1167/1167.mp3"
cd "$(dirname "$0")/.."

case "$lang" in
  zh)
    video=assets/video/ccload-promo.zh-CN.mp4
    voice=zh-CN-YunxiNeural
    rate=+8%
    lines=(
      "0.5|ccLoad，自托管的 AI API 网关。"
      "4.8|只靠一个 Key，限流、故障、成本不透明，迟早被卡住。"
      "11.3|ccLoad 给你一个入口，多条通路。按优先级选择渠道，某条渠道被限流，自动切到下一条。"
      "21.3|任何客户端，任何上游，协议不一致时自动转换。"
      "27.3|每一笔花费都看得见：按渠道、模型和令牌，统计请求、Token 与成本。"
      "34.3|上游凭证留在网关，客户端只拿带限制的 ccLoad 令牌。"
      "39.8|一条命令，几分钟完成部署。"
      "43.7|ccLoad，开源，自托管，现在就试试。"
    )
    ;;
  en)
    video=assets/video/ccload-promo.en.mp4
    voice=en-US-AndrewNeural
    rate=+6%
    lines=(
      "0.5|ccLoad. The self-hosted AI API gateway."
      "4.8|Rely on a single key and you will get stuck: rate limits, outages, and costs you can't see."
      "11.3|ccLoad gives you one entry point and many routes. It picks channels by priority, and when one is rate limited, it fails over to the next."
      "21.3|Any client, any upstream. Protocols are converted on the fly."
      "27.3|Every dollar is visible: requests, tokens and cost, by channel, model and token."
      "34.3|Upstream credentials stay in the gateway. Clients only get a scoped ccLoad token."
      "39.8|Deploy in minutes with a single command."
      "43.7|ccLoad. Open source, self-hosted. Try it today."
    )
    ;;
  *) echo "unknown lang: $lang" >&2; exit 2 ;;
esac

tmp="$(mktemp -d "${TMPDIR:-/tmp}/promo-audio.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

total="$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$video")"
inputs=()
filter=""
mix=""
for i in "${!lines[@]}"; do
  IFS='|' read -r start text <<<"${lines[$i]}"
  # 下一句的起点（最后一句为视频结尾）就是本句的时限。
  end="$total"
  [ $((i + 1)) -lt ${#lines[@]} ] && end="${lines[$((i + 1))]%%|*}"
  uvx edge-tts ${HTTPS_PROXY:+--proxy "$HTTPS_PROXY"} --voice "$voice" --rate="$rate" \
    --text "$text" --write-media "$tmp/$i.raw.mp3" 2>/dev/null
  # edge-tts 的输出带约 0.8s 尾部静音，裁掉后再量时长。
  ffmpeg -v error -y -i "$tmp/$i.raw.mp3" \
    -af "areverse,silenceremove=start_periods=1:start_threshold=-45dB,areverse" "$tmp/$i.wav"
  dur="$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$tmp/$i.wav")"
  awk -v s="$start" -v d="$dur" -v e="$end" -v n="$i" 'BEGIN {
    printf "  #%d %5.1fs + %.2fs → %.2fs (limit %.1fs)%s\n", n, s, d, s + d, e, (s + d > e ? "  ← OVERRUN" : "")
    exit (s + d > e)
  }' || { echo "旁白 #$i 超出场景时长，缩短文案或调快 rate" >&2; exit 1; }
  inputs+=(-i "$tmp/$i.wav")
  ms="$(awk -v s="$start" 'BEGIN { printf "%d", s * 1000 }')"
  filter+="[$((i + 1)):a]adelay=${ms}:all=1[v$i];"
  mix+="[v$i]"
done

# 背景音乐：只取开头与视频等长的一段，归一到 -22 LUFS 后再乘 0.7（约 -3 dB），首尾淡入淡出；
# 旁白出现时由 sidechaincompress 再压低，不和人声抢。
curl -fsSL -A "Mozilla/5.0" -o "$tmp/music.mp3" "$music_url"
fade_out="$(awk -v t="$total" 'BEGIN { printf "%.2f", t - 3 }')"
m=$(( ${#lines[@]} + 1 ))
bed="[$m:a]atrim=0:$total,loudnorm=I=-22:TP=-2:LRA=11,volume=0.7,aresample=48000,"
bed+="afade=t=in:d=1,afade=t=out:st=$fade_out:d=3[bed];"

filter+="${mix}amix=inputs=${#lines[@]}:normalize=0,loudnorm=I=-16:TP=-2:LRA=11,aresample=48000,asplit[voice][key];"
filter+="$bed[bed][key]sidechaincompress=threshold=0.05:ratio=2.5:attack=200:release=800[duck];"
filter+="[voice][duck]amix=inputs=2:normalize=0,alimiter=limit=0.95,apad=whole_dur=$total[a]"

ffmpeg -v error -y -i "$video" "${inputs[@]}" -i "$tmp/music.mp3" -filter_complex "$filter" \
  -map 0:v -map "[a]" -c:v copy -c:a aac -b:a 160k -ac 2 -ar 48000 -t "$total" -movflags +faststart "$tmp/out.mp4"
mv "$tmp/out.mp4" "$video"
echo "$video"
