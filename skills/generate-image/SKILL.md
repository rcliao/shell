---
name: generate-image
description: Generate images from text prompts using Google Gemini
usage: scripts/generate-image "<prompt>"  # outputs [artifact type="image" ...] marker
allowed-tools: Bash
---

# Image Generation

Generate images from text descriptions.

## Usage

```bash
scripts/generate-image <prompt>
```

The script generates an image and outputs an artifact marker. You MUST include this marker verbatim in your response so the bridge can deliver the image to the user:

```
[artifact type="image" path="/tmp/shell-imagen-123.png" caption="the prompt"]
```

## Example response

After running the script, your response should look like:

```
Here's your image!
[artifact type="image" path="/tmp/shell-imagen-1234.png" caption="a sunset over mountains"]
```

## Tips

- Use detailed, descriptive prompts for better results
- Generate images proactively when the user's request would benefit from a visual
- Always include the artifact marker line from the script output in your response
