use std::collections::HashMap;

use anyhow::{Context, Result, bail};
use base64::{Engine, engine::general_purpose::STANDARD};
use chrono::{DateTime, Utc};
use serde::Deserialize;
use takumi::{measure, prelude::*, write_image};

// Lucide icons (ISC): message-circle, repeat-2, heart, matching Starlight.
const REPLY_ICON: &str = r#"<path d="M2.992 16.342a2 2 0 0 1 .094 1.167l-1.065 3.29a1 1 0 0 0 1.236 1.168l3.413-.998a2 2 0 0 1 1.099.092a10 10 0 1 0-4.777-4.719"/>"#;
const REPOST_ICON: &str = r#"<path d="m2 9l3-3l3 3"/><path d="M13 18H7a2 2 0 0 1-2-2V6m17 9l-3 3l-3-3"/><path d="M11 6h6a2 2 0 0 1 2 2v10"/>"#;
const LIKE_ICON: &str = r#"<path d="M2 9.5a5.5 5.5 0 0 1 9.591-3.676a.56.56 0 0 0 .818 0A5.49 5.49 0 0 1 22 9.5c0 2.29-1.5 4-3 5.5l-5.492 5.313a2 2 0 0 1-3 .019L5 15c-1.5-1.5-3-3.2-3-5.5"/>"#;

#[derive(Deserialize)]
#[serde(rename_all = "lowercase")]
enum Theme {
    Light,
    Dark,
}
#[derive(Deserialize)]
pub struct Request {
    theme: Theme,
    post: Post,
    reply_chain: Option<Vec<Post>>,
    has_more_in_chain: Option<bool>,
    images: Option<HashMap<String, String>>,
}
#[derive(Deserialize)]
struct Post {
    author_name: String,
    author_username: String,
    author_avatar_url: String,
    text: String,
    created_timestamp: Option<i64>,
    likes: Option<u64>,
    retweets: Option<u64>,
    replies: Option<u64>,
    media: Option<Media>,
    quote: Option<Box<Post>>,
    article: Option<Article>,
    card: Option<LinkCard>,
}
#[derive(Deserialize)]
struct Media {
    photos: Option<Vec<Image>>,
    videos: Option<Vec<Video>>,
    mosaic: Option<Image>,
    carousel: Option<bool>,
}
#[derive(Deserialize)]
struct Image {
    url: String,
}
#[derive(Deserialize)]
struct Video {
    thumbnail_url: String,
}
#[derive(Deserialize)]
struct Article {
    title: String,
    preview_text: String,
    cover_media: Option<Image>,
}
#[derive(Deserialize)]
struct LinkCard {
    title: String,
    domain: String,
    image: Option<Image>,
}

struct Card<'a> {
    images: &'a HashMap<String, String>,
    text: &'static str,
    secondary: &'static str,
    border: &'static str,
    background: &'static str,
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
fn row(nodes: Vec<Node>, css: &str) -> Node {
    styled(
        Node::container(nodes),
        &format!("display:flex;flex-direction:row;{css}"),
    )
}
fn text(value: impl Into<String>, css: &str) -> Node {
    styled(Node::text(value.into()), css)
}

impl Card<'_> {
    fn stat(&self, icon: &str, count: Option<u64>) -> Node {
        let svg = format!(
            r#"<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="{}" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round">{icon}</svg>"#,
            self.secondary
        );
        row(
            vec![
                styled(
                    Node::image(svg.into_bytes()),
                    "width:16px;height:16px;flex-shrink:0",
                ),
                text(
                    format_count(count),
                    &format!("color:{};font-size:13px", self.secondary),
                ),
            ],
            "align-items:center;gap:6px",
        )
    }

    fn asset(&self, url: &str) -> Result<(Vec<u8>, u32, u32)> {
        let encoded = self
            .images
            .get(url)
            .with_context(|| format!("missing image asset: {url}"))?;
        let bytes = STANDARD
            .decode(encoded)
            .with_context(|| format!("invalid base64 image: {url}"))?;
        let image = image::load_from_memory(&bytes)
            .with_context(|| format!("invalid image asset: {url}"))?;
        Ok((bytes, image.width(), image.height()))
    }
    fn image_node(bytes: Vec<u8>, width: f32, height: f32, radius: f32) -> Node {
        styled(
            Node::image(bytes),
            &format!(
                "width:{width}px;height:{height}px;object-fit:cover;border-radius:{radius}px;flex-shrink:0"
            ),
        )
    }
    fn image(&self, url: &str, width: f32, height: f32, radius: f32) -> Result<Node> {
        let (bytes, _, _) = self.asset(url)?;
        Ok(Self::image_node(bytes, width, height, radius))
    }
    fn photo(&self, url: &str, width: f32) -> Result<Node> {
        let (bytes, source_width, source_height) = self.asset(url)?;
        let height = width * source_height as f32 / source_width as f32;
        Ok(Self::image_node(bytes, width, height, 12.0))
    }
    fn photos(&self, photos: &[Image], width: f32, compact: bool, carousel: bool) -> Result<Node> {
        if carousel || photos.len() > 4 {
            let height = (width * 0.8).min(400.0);
            let mut items = Vec::new();
            for photo in photos {
                let (bytes, w, h) = self.asset(&photo.url)?;
                items.push(Self::image_node(
                    bytes,
                    height * w as f32 / h as f32,
                    height,
                    12.0,
                ));
            }
            let viewport = if compact { width } else { width + 24.0 };
            let radius = if compact { "12px" } else { "12px 0 0 12px" };
            let arrow = r#"<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M5 12h14"/><path d="m12 5 7 7-7 7"/></svg>"#;
            return Ok(column(
                vec![
                    row(items, "gap:4px"),
                    styled(
                        Node::image(arrow.as_bytes().to_vec()),
                        "position:absolute;right:16px;top:50%;margin-top:-16px;width:32px;height:32px;padding:4px;background:rgba(0,0,0,0.6);border-radius:50%",
                    ),
                ],
                &format!(
                    "position:relative;width:{viewport}px;height:{height}px;overflow:hidden;border-radius:{radius};flex-shrink:0"
                ),
            ));
        }
        let mut rows = Vec::new();
        for pair in photos.chunks(2) {
            let item_width = if photos.len() == 1 {
                width
            } else {
                (width - 4.0) / 2.0
            };
            let mut items = Vec::new();
            for image in pair {
                items.push(self.photo(&image.url, item_width)?);
            }
            rows.push(row(items, "gap:4px"));
        }
        Ok(column(rows, "gap:4px"))
    }
    fn avatar(&self, post: &Post, size: f32) -> Result<Node> {
        if post.author_avatar_url.is_empty() {
            return Ok(column(
                vec![],
                &format!(
                    "width:{size}px;height:{size}px;border-radius:100%;background:{};flex-shrink:0",
                    self.secondary
                ),
            ));
        }
        self.image(&post.author_avatar_url, size, size, size / 2.0)
    }
    fn body(&self, value: &str, size: u32) -> Node {
        text(
            value,
            &format!(
                "color:{};font-size:{size}px;line-height:1.4;white-space:pre-wrap;overflow-wrap:anywhere",
                self.text
            ),
        )
    }
    fn post(&self, post: &Post, width: f32, compact: bool) -> Result<Node> {
        let size = if compact { 24.0 } else { 48.0 };
        let timestamp = if compact {
            format_timestamp(post.created_timestamp)?
        } else {
            String::new()
        };
        let header = row(
            vec![
                self.avatar(post, size)?,
                column(
                    vec![
                        text(
                            &post.author_name,
                            &format!("font-size:15px;font-weight:700;color:{}", self.text),
                        ),
                        text(
                            format!(
                                "@{}{}",
                                post.author_username,
                                if timestamp.is_empty() {
                                    String::new()
                                } else {
                                    format!(" · {timestamp}")
                                }
                            ),
                            &format!("font-size:15px;color:{}", self.secondary),
                        ),
                    ],
                    "gap:2px;min-width:0",
                ),
            ],
            "gap:12px;align-items:center",
        );
        let mut nodes = vec![header, self.body(&post.text, if compact { 14 } else { 16 })];
        if let Some(media) = &post.media {
            if let Some(mosaic) = &media.mosaic {
                nodes.push(self.photo(&mosaic.url, width)?);
            } else if let Some(photos) = &media.photos
                && !photos.is_empty()
            {
                nodes.push(self.photos(photos, width, compact, media.carousel.unwrap_or(false))?);
            }
            for video in media.videos.as_deref().unwrap_or_default() {
                nodes.push(column(
                    vec![
                        self.photo(&video.thumbnail_url, width)?,
                        text(
                            "▶",
                            "position:absolute;top:42%;left:45%;font-size:32px;color:white;background:rgba(0,0,0,0.7);border-radius:30px;padding:8px 12px",
                        ),
                    ],
                    "position:relative",
                ));
            }
        }
        if let Some(article) = &post.article {
            let mut content = Vec::new();
            if let Some(image) = &article.cover_media {
                content.push(self.photo(&image.url, width - 24.0)?);
            }
            content.push(self.body(&article.title, 15));
            content.push(self.body(&article.preview_text, 14));
            nodes.push(column(
                content,
                &format!(
                    "border:1px solid {};border-radius:12px;padding:12px;gap:4px",
                    self.border
                ),
            ));
        }
        if let Some(card) = &post.card {
            let mut content = Vec::new();
            if let Some(image) = &card.image {
                content.push(self.photo(&image.url, width - 24.0)?);
            }
            content.push(self.body(&card.title, 14));
            content.push(text(
                format!("From {}", card.domain),
                &format!("font-size:13px;color:{}", self.secondary),
            ));
            nodes.push(column(
                content,
                &format!(
                    "border:1px solid {};border-radius:12px;padding:12px;gap:4px",
                    self.border
                ),
            ));
        }
        if let Some(quote) = &post.quote {
            nodes.push(column(
                vec![self.post(quote, width - 24.0, true)?],
                &format!(
                    "border:1px solid {};border-radius:12px;padding:12px",
                    self.border
                ),
            ));
        }
        Ok(column(nodes, "gap:12px;min-width:0"))
    }
}

fn format_timestamp(timestamp: Option<i64>) -> Result<String> {
    Ok(timestamp
        .map(|t| DateTime::<Utc>::from_timestamp(t, 0).context("invalid UTC timestamp"))
        .transpose()?
        .map(|d| d.format("%-I:%M %p · %b %-d, %Y UTC").to_string())
        .unwrap_or_default())
}

fn format_count(count: Option<u64>) -> String {
    let count = count.unwrap_or(0);
    if count >= 1_000_000 {
        format!("{:.1}M", count as f64 / 1_000_000.0)
    } else if count >= 1_000 {
        format!("{:.1}K", count as f64 / 1_000.0)
    } else {
        count.to_string()
    }
}

pub fn render(request: Request) -> Result<Vec<u8>> {
    let empty = HashMap::new();
    let (background, text_color, secondary, border) = match request.theme {
        Theme::Light => ("#f7f9f9", "#0f1419", "#536471", "#cfd9de"),
        Theme::Dark => ("#16181c", "#e7e9ea", "#7c8186", "#3e4144"),
    };
    let card = Card {
        images: request.images.as_ref().unwrap_or(&empty),
        text: text_color,
        secondary,
        border,
        background,
    };
    let mut nodes = Vec::new();
    if request.has_more_in_chain.unwrap_or(false) {
        nodes.push(text("⋮", &format!("color:{secondary};font-size:24px")));
    }
    for reply in request.reply_chain.as_deref().unwrap_or_default() {
        nodes.push(card.post(reply, 502.0, true)?);
    }
    nodes.push(card.post(&request.post, 502.0, false)?);
    let date = format_timestamp(request.post.created_timestamp)?;
    nodes.push(row(
        vec![
            text(date, &format!("color:{secondary};font-size:13px")),
            row(
                vec![
                    card.stat(REPLY_ICON, request.post.replies),
                    card.stat(REPOST_ICON, request.post.retweets),
                    card.stat(LIKE_ICON, request.post.likes),
                ],
                "gap:16px;margin-left:auto",
            ),
        ],
        "margin-top:10px;gap:8px;align-items:center",
    ));
    let root = column(
        nodes,
        &format!(
            "width:550px;padding:24px;background:{};gap:12px",
            card.background
        ),
    );
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
    let viewport = Viewport::new((Some(1100), None)).with_device_pixel_ratio(2.0);
    let options = RenderOptions::builder()
        .viewport(viewport)
        .node(root)
        .fonts(&fonts)
        .build();
    let measured = measure(options.clone()).context("measure card")?;
    if measured.height.ceil() > 8900.0 {
        bail!(
            "card too tall for Telegram photo without cropping text: {} pixels",
            measured.height.ceil()
        );
    }
    let bitmap = takumi::render(options).context("render card")?;
    let mut jpeg = Vec::new();
    write_image(
        &bitmap,
        &mut jpeg,
        OutputFormat::Jpeg {
            quality: Quality::new(85),
        },
    )
    .context("encode JPEG")?;
    if jpeg.len() > 10_000_000 {
        bail!("JPEG exceeds Telegram's 10 MB photo limit");
    }
    Ok(jpeg)
}
