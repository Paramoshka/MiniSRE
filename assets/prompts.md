# Image prompts

Generated with the built-in `imagegen` tool. The cover uses `telegram-avatar.png` as its character reference.

## Telegram avatar

```text
Use case: stylized-concept
Asset type: square Telegram bot avatar for the mini-sre repository
Primary request: a beautiful, distinctive friendly robot on-call SRE mascot.
Subject: one compact expressive robot, head and upper body, with a simple server-status indicator integrated into its chest. Competent, calm and approachable.
Style/medium: polished stylized 3D illustration, clean sculpted forms, tactile finish, restrained technical detailing.
Composition/framing: square 1024x1024 composition; centered close-up, large readable face and strong silhouette, generous margin so the entire face remains visible in a circular Telegram crop. Works clearly at 64 pixels.
Scene/backdrop: simple uncluttered background with strong separation from the robot.
Lighting/mood: soft studio lighting, welcoming night-shift companion.
Constraints: a single robot; no text, letters, watermark or unrelated objects. Produce a finished raster image, not a mockup of a messaging app.
```

## README cover

```text
Use case: stylized-concept
Asset type: wide GitHub README cover for mini-sre
Input images: Image 1 is the identity and visual-style reference for the robot mascot.
Primary request: create a beautiful wide landscape repository cover with the SAME friendly on-call SRE robot and the exact project title "mini-sre".
Subject: preserve the referenced robot's white rounded casing, dark face panel, expressive cyan eyes and smile, single glowing antenna and small server-status chest indicators. Same polished 3D material finish and lighting character.
Scene/backdrop: simple spacious dark backdrop consistent with the reference, restrained soft technical glow, no unrelated props.
Composition/framing: wide landscape banner approximately 2.5:1; robot's head and upper body occupy one side, with generous separate space for a large readable project title on the other side. Keep all key elements inside comfortable margins. Balanced and uncluttered.
Text (verbatim): "mini-sre" — only this text, lowercase, exact hyphen, large clean sans-serif typography.
Constraints: retain the robot identity from Image 1; no extra characters, subtitles, slogans, watermark or brand logos. Finished raster artwork, not a GitHub screenshot.
```

## README cover: MiniSRE name update

Edited with the built-in `imagegen` tool using the existing README cover as the edit target.

```text
Use case: text-localization. Edit target: the provided existing MiniSRE GitHub README cover. Change ONLY the project title from "mini-sre" to exactly "MiniSRE" (M uppercase, ini lowercase, SRE uppercase; no hyphen, no spaces). Keep the existing robot mascot, thumbs-up pose, facial expression, casing, chest indicators, antenna, background, glowing ring, lighting, colors, composition and wide approximately 2.5:1 aspect ratio unchanged. Preserve the large clean sans-serif typography on the right, with "Mini" in white and "SRE" in cyan and the existing subtle glow. Fit the title inside the existing right-hand title area with comfortable margins. No other text, no new objects, no watermark. Return one finished replacement raster banner. Save the resulting image and return its local filesystem path so it can replace the project asset.
```
