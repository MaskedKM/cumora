// One-shot asset optimizer (#144a). Convergent, not strictly idempotent:
// re-quantizing an already-quantized PNG can still shave a few percent,
// so the write guard requires BOTH ≥10% AND ≥2KB reduction — run-to-run
// churn stops once files sit within that margin of the fixed point.
//
//   node scripts/optimize-assets.mjs
//
// Targets — see issue #144 for the why (18MB of public/ riding along in
// every Electron/Capacitor package, a 2.1MB PNG rendered at 16px, 9MB of
// 1024px starter avatars that only ever display at chip size):
//   - everyone.png            1024px → 96px  (max render is 28px; 3× retina)
//   - starter-avatars/*.png   1024px → 256px (avatar renders ≤ ~96px)
//   - skype-emojis/anim/*.png dimensions UNTOUCHED (vertical stacks of
//     40x40 frames; the CSS steps() animation math depends on exact
//     sheet geometry) — palette re-encode only.
// Filenames and paths never change, so server references
// (internal/onboard) and every <img src> keep working untouched.
import sharp from 'sharp'
import fs from 'node:fs'
import path from 'node:path'

const PNG = { palette: true, quality: 90, compressionLevel: 9 }

const JOBS = [
  { file: 'apps/web/public/everyone.png', resize: 96 },
  { dir: 'apps/web/public/starter-avatars', resize: 256 },
  { dir: 'apps/web/public/skype-emojis/anim', resize: null },
]

let totalBefore = 0
let totalAfter = 0
for (const job of JOBS) {
  const files = job.file
    ? [job.file]
    : fs.readdirSync(job.dir).filter((f) => f.endsWith('.png')).map((f) => path.join(job.dir, f))
  for (const file of files) {
    const before = fs.statSync(file).size
    let img = sharp(file)
    const meta = await img.metadata()
    if (job.resize && meta.width && meta.width > job.resize) {
      img = img.resize(job.resize, job.resize, { fit: 'inside' })
    }
    const out = await img.png(PNG).toBuffer()
    if (out.length < before * 0.9 && out.length < before - 2048) {
      fs.writeFileSync(file, out)
      console.log(`${file}  ${(before / 1024).toFixed(1)}KB → ${(out.length / 1024).toFixed(1)}KB`)
      totalBefore += before
      totalAfter += out.length
    } else {
      console.log(`${file}  ${(before / 1024).toFixed(1)}KB (kept)`)
      totalBefore += before
      totalAfter += before
    }
  }
}
console.log(`total ${(totalBefore / 1024 / 1024).toFixed(1)}MB → ${(totalAfter / 1024 / 1024).toFixed(1)}MB`)
