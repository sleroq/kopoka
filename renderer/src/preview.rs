use anyhow::{Context, Result};
use base64::{Engine, engine::general_purpose::STANDARD};
use serde::Deserialize;
use takumi::{prelude::*, write_image};

#[derive(Deserialize)]
pub struct Preview {
    bot: String,
    blocks: Vec<Block>,
}

#[derive(Deserialize)]
#[serde(tag = "kind", rename_all = "lowercase")]
enum Block {
    Text {
        text: String,
    },
    Author {
        text: String,
    },
    Quote {
        text: String,
    },
    Media {
        image: String,
        video: bool,
        count: usize,
    },
    Caption {
        text: String,
    },
}

fn styled(node: Node, css: &str) -> Node {
    node.with_style(StyleDeclarationBlock::parse_loosy(css).into())
}

fn column(nodes: Vec<Node>, css: &str) -> Node {
    styled(
        Node::container(nodes),
        &format!("display:flex;flex-direction:column;{css}"),
    )
}

fn text(value: String, css: &str) -> Node {
    styled(
        Node::text(value),
        &format!(
            "font-size:15px;line-height:1.35;white-space:pre-wrap;overflow-wrap:anywhere;{css}"
        ),
    )
}

fn limited(value: &str, budget: &mut usize) -> String {
    let mut result = String::new();
    let mut lines = 0;
    let mut chars = value.chars();
    while *budget > 0 {
        let Some(c) = chars.next() else { return result };
        if c == '\n' {
            lines += 1;
            if lines >= 10 {
                break;
            }
        }
        result.push(c);
        *budget -= 1;
    }
    if !result.is_empty() {
        result.push('…');
    }
    result
}

fn media(encoded: &str, video: bool, count: usize) -> Result<Node> {
    let bytes = STANDARD
        .decode(encoded)
        .context("invalid preview image base64")?;
    let image = image::load_from_memory(&bytes).context("invalid preview image")?;
    let height = (320.0 * image.height() as f32 / image.width() as f32).clamp(70.0, 400.0);
    let mut nodes = vec![styled(
        Node::image(bytes),
        &format!("width:320px;height:{height}px;object-fit:contain;flex-shrink:0"),
    )];
    if video {
        let play = r##"<svg xmlns="http://www.w3.org/2000/svg" width="52" height="52" viewBox="0 0 52 52"><circle cx="26" cy="26" r="25" fill="#14212b" fill-opacity="0.78"/><path d="M21 15v22l17-11z" fill="white"/></svg>"##;
        nodes.push(styled(Node::image(play.as_bytes().to_vec()), "position:absolute;left:50%;top:50%;margin-left:-26px;margin-top:-26px;width:52px;height:52px"));
    }
    if count > 1 {
        let arrow = r##"<svg xmlns="http://www.w3.org/2000/svg" width="32" height="32" viewBox="0 0 32 32"><circle cx="16" cy="16" r="16" fill="#14212b" fill-opacity="0.75"/><path d="m13 9 7 7-7 7" fill="none" stroke="white" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"/></svg>"##;
        nodes.push(styled(
            Node::image(arrow.as_bytes().to_vec()),
            "position:absolute;right:8px;top:50%;margin-top:-16px;width:32px;height:32px",
        ));
        let dots: Vec<_> = (0..count.min(7))
            .map(|i| {
                styled(
                    Node::container(vec![]),
                    if i == 0 {
                        "width:6px;height:6px;border-radius:3px;background:white"
                    } else {
                        "width:6px;height:6px;border-radius:3px;background:#aab5bd"
                    },
                )
            })
            .collect();
        nodes.push(styled(Node::container(dots), "position:absolute;bottom:8px;left:50%;transform:translateX(-50%);display:flex;flex-direction:row;gap:5px;padding:5px 7px;background:rgba(20,33,43,0.75);border-radius:10px"));
    }
    Ok(column(
        nodes,
        &format!(
            "position:relative;width:320px;height:{height}px;background:#18252e;border-radius:8px;overflow:hidden;flex-shrink:0"
        ),
    ))
}

pub fn render(preview: Preview) -> Result<Vec<u8>> {
    let mut nodes = Vec::new();
    if !preview.bot.is_empty() {
        nodes.push(text(
            format!("via @{}", preview.bot),
            "color:#73b9e8;font-size:13px;font-weight:bold",
        ));
    }
    let mut budget = 1400;
    for block in preview.blocks {
        let node = match block {
            Block::Text { text: value } => text(limited(&value, &mut budget), "color:#f1f4f6"),
            Block::Author { text: value } => text(
                limited(&value, &mut budget),
                "color:#75bde9;font-weight:bold",
            ),
            Block::Quote { text: value } => column(
                vec![text(limited(&value, &mut budget), "color:#d9e5ec")],
                "border-left:3px solid #6ab7e9;padding-left:10px",
            ),
            Block::Media {
                image,
                video,
                count,
            } => media(&image, video, count)?,
            Block::Caption { text: value } => text(
                limited(&value, &mut budget),
                "color:#75bde9;text-decoration:underline",
            ),
        };
        nodes.push(node);
    }
    let bubble = column(
        nodes,
        "width:344px;padding:12px;gap:8px;background:#253743;border:1px solid #405563;border-radius:14px",
    );
    let root = column(vec![bubble], "width:384px;padding:20px;background:#17252f");
    let mut fonts = Fonts::default();
    for bytes in [
        include_bytes!("../assets/fonts/Inter-Regular.ttf").as_slice(),
        include_bytes!("../assets/fonts/Inter-Bold.ttf").as_slice(),
        include_bytes!("../assets/fonts/NotoSans-Regular.ttf").as_slice(),
        include_bytes!("../assets/fonts/NotoSansCJKsc-Regular.otf").as_slice(),
        include_bytes!("../assets/fonts/NotoSansMath-Regular.ttf").as_slice(),
        include_bytes!("../assets/fonts/NotoColorEmoji.ttf").as_slice(),
    ] {
        fonts
            .register(FontResource::new(bytes))
            .context("register embedded font")?;
    }
    let options = RenderOptions::builder()
        .viewport(Viewport::new((Some(768), None)).with_device_pixel_ratio(2.0))
        .node(root)
        .fonts(&fonts)
        .build();
    let bitmap = takumi::render(options).context("render preview")?;
    let mut output = Vec::new();
    write_image(
        &bitmap,
        &mut output,
        OutputFormat::Jpeg {
            quality: Quality::new(85),
        },
    )
    .context("encode preview JPEG")?;
    Ok(output)
}
