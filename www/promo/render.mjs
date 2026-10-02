// 逐帧渲染 promo/index.html，输出 JPEG 序列，再由 ffmpeg 合成 mp4。
//
//   (cd www && python3 -m http.server 8765 &)
//   ego-browser nodejs -e "globalThis.PROMO={lang:'zh',out:'/tmp/promo-zh'};$(cat www/promo/render.mjs)"
//   ffmpeg -framerate 30 -i /tmp/promo-zh/f%05d.jpg -c:v libx264 -crf 18 -preset slow \
//     -pix_fmt yuv420p -movflags +faststart www/assets/video/ccload-promo.zh-CN.mp4
//
// PROMO.at = [2, 8, 16] 只截取指定秒数的画面（检查用）。
const { mkdir, writeFile } = await import("node:fs/promises");

const { lang = "zh", out, at } = globalThis.PROMO || {};
const fps = 30;
if (!out) throw new Error("PROMO.out is required");
await mkdir(out, { recursive: true });

const task = await taskSpace(`render ccLoad promo ${lang}`);
const page = task.page("p1");
await page.cdp("Emulation.setDeviceMetricsOverride", { width: 1920, height: 1080, deviceScaleFactor: 1, mobile: false });
await page.goto(`http://127.0.0.1:8765/promo/index.html?lang=${lang}&capture=1`);
await page.waitForFunction(() => typeof window.seek === "function");

const total = await page.evaluate(() => window.TOTAL);
const times = at
  ? at
  : Array.from({ length: Math.round(total * fps) }, (_, i) => i / fps);

for (const [i, t] of times.entries()) {
  await page.evaluate((time) => window.seek(time), t);
  const shot = await page.cdp("Page.captureScreenshot", { format: "jpeg", quality: 93 });
  await writeFile(`${out}/f${String(i).padStart(5, "0")}.jpg`, Buffer.from(shot.data, "base64"));
}

await task.finish({ keep: [] });
console.log({ lang, frames: times.length, out });
