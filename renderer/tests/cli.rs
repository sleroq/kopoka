use std::{
    io::Write,
    process::{Command, Output, Stdio},
};

use base64::{Engine, engine::general_purpose::STANDARD};
use image::{DynamicImage, ImageFormat, Rgb, RgbImage};
use serde_json::{Value, json};

fn asset(width: u32, height: u32, color: [u8; 3]) -> String {
    let image = DynamicImage::ImageRgb8(RgbImage::from_pixel(width, height, Rgb(color)));
    let mut bytes = std::io::Cursor::new(Vec::new());
    image.write_to(&mut bytes, ImageFormat::Png).unwrap();
    STANDARD.encode(bytes.into_inner())
}

fn render(media: Value, images: Value) -> Output {
    let request = json!({
        "theme": "light",
        "post": {"author_name": "A", "author_username": "a", "author_avatar_url": "", "text": "Hello", "media": media},
        "images": images
    });
    render_request(request)
}

fn render_request(request: Value) -> Output {
    let mut child = Command::new(env!("CARGO_BIN_EXE_kopoka-render"))
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    child
        .stdin
        .take()
        .unwrap()
        .write_all(request.to_string().as_bytes())
        .unwrap();
    child.wait_with_output().unwrap()
}

fn height(output: &Output) -> u32 {
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    image::load_from_memory(&output.stdout).unwrap().height()
}

#[test]
fn cli_renders_polls_in_posts_quotes_and_replies() {
    let poll = json!({"choices":[{"label":"A long choice that wraps inside the narrower quoted and reply posts when needed", "percentage":55.2},{"label":"Other", "percentage":18.0}], "total_votes":1019, "time_left_en":"21 hours left"});
    let post =
        json!({"author_name":"A", "author_username":"a", "author_avatar_url":"", "text":"Hello"});
    for theme in ["light", "warm", "dark", "dim"] {
        let mut with_poll = post.clone();
        with_poll["poll"] = poll.clone();
        let cases = [
            (
                json!({"theme":theme,"post":post}),
                json!({"theme":theme,"post":with_poll}),
            ),
            (
                json!({"theme":theme,"post":{ "author_name":"A", "author_username":"a", "author_avatar_url":"", "text":"Hello", "quote":post }}),
                json!({"theme":theme,"post":{ "author_name":"A", "author_username":"a", "author_avatar_url":"", "text":"Hello", "quote":with_poll }}),
            ),
            (
                json!({"theme":theme,"post":post,"reply_chain":[post]}),
                json!({"theme":theme,"post":post,"reply_chain":[with_poll]}),
            ),
        ];
        for (index, (plain, polled)) in cases.into_iter().enumerate() {
            let plain = render_request(plain);
            let polled = render_request(polled);
            assert!(height(&polled) > height(&plain) + 100, "{theme}");
            assert_ne!(plain.stdout, polled.stdout);
            if theme == "light" && index == 0 {
                let image = image::load_from_memory(&polled.stdout).unwrap().to_rgb8();
                let is_fill = |p: &Rgb<u8>| {
                    p.0.iter()
                        .zip([207, 217, 222])
                        .all(|(a, b)| a.abs_diff(b) < 10)
                };
                let top = (180..image.height())
                    .find(|&y| is_fill(image.get_pixel(100, y)))
                    .unwrap();
                assert!(is_fill(image.get_pixel(300, top + 8)));
                assert!(!is_fill(image.get_pixel(700, top + 8)));
                assert!(
                    (top + 15..top + 100)
                        .any(|y| (70..500).any(|x| image.get_pixel(x, y)[0] < 100)),
                    "label must appear over the fill"
                );
            }
        }
    }
}

#[test]
fn cli_renders_theme_palettes() {
    for (theme, background) in [
        ("light", [255, 255, 255]),
        ("warm", [255, 246, 245]),
        ("dark", [0, 0, 0]),
        ("dim", [22, 24, 28]),
    ] {
        let output = render_request(json!({
            "theme": theme,
            "post": {"author_name": "A", "author_username": "a", "author_avatar_url": "", "text": "Hello"}
        }));
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
        let image = image::load_from_memory(&output.stdout).unwrap().to_rgb8();
        let pixel = image.get_pixel(4, 4).0;
        assert!(
            pixel
                .iter()
                .zip(background)
                .all(|(actual, expected)| actual.abs_diff(expected) <= 3),
            "{theme}: {pixel:?}"
        );
    }
}

#[test]
fn cli_preserves_media_aspect_and_includes_all_posters() {
    let images = json!({"tall": asset(10, 30, [220, 30, 30]), "wide": asset(30, 3, [30, 220, 30]), "video1": asset(10, 10, [30, 30, 220]), "video2": asset(10, 10, [220, 220, 30])});
    let empty_height = height(&render(json!(null), images.clone()));
    let tall_height = height(&render(
        json!({"photos": [{"url": "tall"}]}),
        images.clone(),
    ));
    let wide_height = height(&render(
        json!({"photos": [{"url": "wide"}]}),
        images.clone(),
    ));
    assert!(tall_height - empty_height > 3000, "tall photo was cropped");
    assert!(wide_height - empty_height < 150, "panorama was cropped");

    let photo = json!({"photos": [{"url": "wide"}]});
    let mixed = json!({"photos": [{"url": "wide"}], "videos": [
        {"thumbnail_url": "video1"}, {"thumbnail_url": "video2"}
    ]});
    let photo_output = render(photo, images.clone());
    let mixed_output = render(mixed.clone(), images.clone());
    assert!(height(&mixed_output) > height(&photo_output) + 2000);
    assert_ne!(mixed_output.stdout, photo_output.stdout);

    let mut missing = images;
    missing.as_object_mut().unwrap().remove("video2");
    let output = render(mixed, missing);
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("video2"));
}

#[test]
fn cli_carousel_clips_at_card_edge_with_arrow() {
    let images =
        json!({"red": asset(30, 40, [220, 30, 30]), "green": asset(30, 40, [30, 220, 30])});
    let output = render(
        json!({"carousel": true, "photos": [{"url":"red"},{"url":"green"}]}),
        images,
    );
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let img = image::load_from_memory(&output.stdout).unwrap().to_rgb8();
    // Locate the red row, then check its height, next panel and overlay at the right edge.
    let red = |p: &Rgb<u8>| p[0] > 180 && p[1] < 80;
    let ys: Vec<_> = (0..img.height())
        .filter(|&y| red(img.get_pixel(100, y)))
        .collect();
    assert!(!ys.is_empty());
    let top = ys[0];
    let bottom = *ys.last().unwrap();
    assert!(
        (790..=805).contains(&(bottom - top)),
        "row height {}",
        bottom - top
    );
    let middle = (top + bottom) / 2;
    assert!(red(img.get_pixel(200, middle)));
    let green = img.get_pixel(1000, middle - 100);
    assert!(green[1] > 180 && green[0] < 80, "next panel {green:?}");
    let edge = img.get_pixel(1098, middle - 100);
    assert!(edge[1] > 180 && edge[0] < 80, "right edge {edge:?}");
    let overlay = img.get_pixel(1036, middle);
    assert!(
        overlay[1] < 180 || overlay[0] > 100,
        "arrow overlay {overlay:?}"
    );
}

#[test]
fn cli_renders_supplied_timestamps() {
    let render_dates = |date: &str| {
        let post = json!({"author_name":"A", "author_username":"a", "author_avatar_url":"", "text":"Hello", "created_at":date});
        let request = json!({"theme":"light", "post":post, "reply_chain":[post]});
        let mut child = Command::new(env!("CARGO_BIN_EXE_kopoka-render"))
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .spawn()
            .unwrap();
        child
            .stdin
            .take()
            .unwrap()
            .write_all(request.to_string().as_bytes())
            .unwrap();
        let output = child.wait_with_output().unwrap();
        assert!(output.status.success());
        output.stdout
    };
    assert_ne!(
        render_dates("2:30 AM · Jan 1, 2024 UTC+03:00"),
        render_dates("6:30 PM · Dec 31, 2023 UTC-05:00")
    );
}

#[test]
fn cli_bars_match_theme_background() {
    let images =
        json!({"red": asset(10, 40, [220, 30, 30]), "green": asset(10, 40, [30, 220, 30])});
    let photos = json!([{"url":"red"}, {"url":"green"}]);
    for (theme, background) in [
        ("light", [255, 255, 255]),
        ("warm", [255, 246, 245]),
        ("dark", [0, 0, 0]),
        ("dim", [22, 24, 28]),
    ] {
        for bars in [false, true] {
            let output = render_request(json!({
                "theme": theme,
                "post": {"author_name":"A", "author_username":"a", "author_avatar_url":"", "text":"Hello", "media":{"carousel":true,"bars":bars,"photos":photos}},
                "images": images
            }));
            assert!(
                output.status.success(),
                "{}",
                String::from_utf8_lossy(&output.stderr)
            );
            let image = image::load_from_memory(&output.stdout).unwrap().to_rgb8();
            let is_red = |p: &Rgb<u8>| p[0] > 180 && p[1] < 80;
            let is_green = |p: &Rgb<u8>| p[1] > 180 && p[0] < 80;
            let ys: Vec<_> = (0..image.height())
                .filter(|&y| is_red(image.get_pixel(100, y)))
                .collect();
            assert!(!ys.is_empty());
            let y = (ys[0] + ys[ys.len() - 1]) / 2;
            let red_end = (0..image.width())
                .rfind(|&x| is_red(image.get_pixel(x, y)))
                .unwrap();
            let green_start = (red_end + 1..image.width())
                .find(|&x| is_green(image.get_pixel(x, y)))
                .unwrap();
            let green_end = (green_start..image.width())
                .rfind(|&x| is_green(image.get_pixel(x, y)))
                .unwrap();
            let matches_background = |pixel: &Rgb<u8>| {
                pixel
                    .0
                    .iter()
                    .zip(background)
                    .all(|(actual, expected)| actual.abs_diff(expected) <= 35)
            };
            let gap = image.get_pixel((red_end + green_start) / 2, y);
            assert!(matches_background(gap), "{theme} bars={bars} gap {gap:?}");
            for y in [ys[0] + 1, ys[ys.len() - 1] - 1] {
                let corner = image.get_pixel(green_end - 1, y);
                assert!(
                    matches_background(corner),
                    "{theme} bars={bars} corner {corner:?}"
                );
            }
            let beyond = image.get_pixel(green_end + 30, y);
            assert!(
                matches_background(beyond),
                "{theme} bars={bars} after photos {beyond:?}"
            );
        }
    }
}

#[test]
fn cli_reply_rail_and_media_are_bounded() {
    for theme in ["light", "dark"] {
        let parent = json!({"author_name":"Parent", "author_username":"parent", "author_avatar_url":"avatar", "text":"Reply", "media":{"carousel":true,"photos":[{"url":"media"},{"url":"media"}]}, "quote":{"author_name":"Quote", "author_username":"quote", "author_avatar_url":"", "text":"Quoted", "media":{"photos":[{"url":"media"}]}}});
        let short = json!({"author_name":"Short", "author_username":"short", "author_avatar_url":"avatar", "text":"Reply"});
        let main = json!({"author_name":"Main", "author_username":"main", "author_avatar_url":"", "text":"Main"});
        let images =
            json!({"avatar":asset(20, 20, [230, 20, 20]), "media":asset(20, 20, [20, 220, 20])});
        let output = render_request(
            json!({"theme":theme,"post":main,"reply_chain":[parent,short],"has_more_in_chain":true,"images":images}),
        );
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
        let img = image::load_from_memory(&output.stdout).unwrap().to_rgb8();
        let red = |p: &Rgb<u8>| p[0] > 180 && p[1] < 70;
        let green = |p: &Rgb<u8>| p[1] > 170 && p[0] < 80;
        // Root padding is 24px, dots occupy 32px, and the avatar is 48px square.
        assert!(red(img.get_pixel(96, 160)));
        assert!(!red(img.get_pixel(146, 160)));
        let media_y = (160..img.height())
            .find(|&y| green(img.get_pixel(200, y)))
            .unwrap();
        assert!(!green(img.get_pixel(160, media_y)));
        assert!(green(img.get_pixel(1030, media_y)));
        assert!(!green(img.get_pixel(1080, media_y)));
        // Rail continues beside media, from below the first avatar to the next one.
        let rail = img.get_pixel(94, media_y + 100);
        let background = img.get_pixel(80, media_y + 100);
        assert_ne!(rail, background);
        let second_y = (media_y + 1..img.height())
            .find(|&y| red(img.get_pixel(96, y)))
            .unwrap();
        assert_ne!(
            img.get_pixel(94, second_y - 14),
            img.get_pixel(80, second_y - 14)
        );
        // Three dots sit above the avatar on the same rail.
        for y in [60, 80, 100] {
            assert_ne!(img.get_pixel(96, y), img.get_pixel(80, y));
        }
        let standalone =
            render_request(json!({"theme":theme,"post":main,"has_more_in_chain":true}));
        let no_dots = render_request(json!({"theme":theme,"post":main}));
        assert_eq!(standalone.stdout, no_dots.stdout);
    }
}

#[test]
fn cli_scene_exports_ordered_motion_geometry() {
    let items = json!([
        {"url":"red", "type":"photo"},
        {"url":"animation", "type":"gif", "thumbnail_url":"green"},
        {"url":"blue", "type":"photo"},
        {"url":"animation", "type":"video", "thumbnail_url":"green"}
    ]);
    let media =
        json!({"items":items,"photos":[{"url":"missing"}],"videos":[{"thumbnail_url":"missing"}]});
    let post = json!({"author_name":"A","author_username":"a","author_avatar_url":"","text":"Hello", "media":media});
    let images = json!({"red":asset(10,10,[220,30,30]),"green":asset(10,10,[30,220,30]),"blue":asset(10,10,[30,30,220])});
    for bars in [false, true] {
        let mut post = post.clone();
        post["media"]["bars"] = json!(bars);
        let output =
            render_request(json!({"theme":"light","scene":true,"post":post,"images":images}));
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
        let scene: Value = serde_json::from_slice(&output.stdout).unwrap();
        let image =
            image::load_from_memory(&STANDARD.decode(scene["image"].as_str().unwrap()).unwrap())
                .unwrap()
                .to_rgb8();
        let slots = scene["slots"].as_array().unwrap();
        assert_eq!(slots.len(), 2);
        assert_eq!(slots[0]["url"], "animation");
        assert_eq!(slots[0]["kind"], "gif");
        assert_eq!(slots[1]["kind"], "video");
        for slot in slots {
            let x = slot["x"].as_u64().unwrap() as u32;
            let y = slot["y"].as_u64().unwrap() as u32;
            let w = slot["width"].as_u64().unwrap() as u32;
            let h = slot["height"].as_u64().unwrap() as u32;
            assert_eq!(slot["radius"], if bars { 24 } else { 0 });
            assert_eq!(image.get_pixel(x + w / 2, y + h / 2).0, [30, 220, 30]);
            assert_eq!(image.get_pixel(x + w / 2, y + 1).0, [30, 220, 30]);
            assert_eq!(image.get_pixel(x + w / 2, y - 1).0, [255, 255, 255]);
            assert_eq!(image.get_pixel(x + w / 2, y + h).0, [255, 255, 255]);
            if bars {
                assert_eq!(image.get_pixel(x, y).0, [255, 255, 255]);
                assert_eq!(image.get_pixel(x - 4, y + h / 2).0, [255, 255, 255]);
            }
        }
        let first = &slots[0];
        let x = first["x"].as_u64().unwrap() as u32;
        let y = first["y"].as_u64().unwrap() as u32 + first["height"].as_u64().unwrap() as u32 / 2;
        assert_eq!(
            image.get_pixel(x - if bars { 12 } else { 1 }, y).0,
            [220, 30, 30]
        );
        let right = x + first["width"].as_u64().unwrap() as u32 + if bars { 12 } else { 1 };
        assert_eq!(image.get_pixel(right, y).0, [30, 30, 220]);
    }
}

#[test]
fn cli_scene_motion_panels_share_rounded_pixel_boundaries() {
    let items = json!([
        {"url":"a", "type":"gif", "thumbnail_url":"a", "fallback_urls":["alternate"]},
        {"url":"b", "type":"gif", "thumbnail_url":"b"},
        {"url":"c", "type":"gif", "thumbnail_url":"c"}
    ]);
    let output = render_request(json!({
        "theme":"light", "scene":true,
        "post":{"author_name":"A", "author_username":"a", "author_avatar_url":"", "text":"Hello", "media":{"items":items}},
        "images":{"a":asset(77,95,[220,30,30]),"b":asset(55,20,[30,220,30]),"c":asset(40,74,[30,30,220])}
    }));
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let scene: Value = serde_json::from_slice(&output.stdout).unwrap();
    let slots = scene["slots"].as_array().unwrap();
    assert_eq!(slots.len(), 3);
    assert_eq!(slots[0]["fallback_urls"], json!(["alternate"]));
    for pair in slots.windows(2) {
        assert_eq!(
            pair[0]["x"].as_i64().unwrap() + pair[0]["width"].as_i64().unwrap(),
            pair[1]["x"].as_i64().unwrap()
        );
        assert_eq!(pair[0]["y"], pair[1]["y"]);
        assert_eq!(pair[0]["height"], pair[1]["height"]);
    }
}

#[test]
fn cli_scene_maps_reply_and_nested_quote_occurrences() {
    let motion = json!({"items":[{"url":"same","type":"gif","thumbnail_url":"green"}]});
    let post = json!({"author_name":"A","author_username":"a","author_avatar_url":"","text":"Hello","media":motion});
    let mut reply = post.clone();
    reply["quote"] = post.clone();
    let output = render_request(
        json!({"theme":"light","scene":true,"post":post,"reply_chain":[reply],"images":{"green":asset(40,10,[30,220,30])}}),
    );
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let scene: Value = serde_json::from_slice(&output.stdout).unwrap();
    let image =
        image::load_from_memory(&STANDARD.decode(scene["image"].as_str().unwrap()).unwrap())
            .unwrap()
            .to_rgb8();
    let slots = scene["slots"].as_array().unwrap();
    assert_eq!(slots.len(), 3);
    let mut last_y = 0;
    for slot in slots {
        let x = slot["x"].as_u64().unwrap() as u32;
        let y = slot["y"].as_u64().unwrap() as u32;
        let w = slot["width"].as_u64().unwrap() as u32;
        let h = slot["height"].as_u64().unwrap() as u32;
        assert!(y > last_y);
        last_y = y;
        assert_eq!(slot["url"], "same");
        assert_eq!(slot["radius"], 24);
        assert_eq!(image.get_pixel(x + w / 2, y + h / 2).0, [30, 220, 30]);
        assert_eq!(image.get_pixel(x + w / 2, y - 1).0, [255, 255, 255]);
        assert_eq!(image.get_pixel(x + w / 2, y + 1).0, [30, 220, 30]);
    }
    assert!(slots[1]["x"].as_u64().unwrap() > slots[0]["x"].as_u64().unwrap());
    assert!(slots[2]["x"].as_u64().unwrap() < slots[0]["x"].as_u64().unwrap());
}

#[test]
fn cli_renders_preview_media_and_controls() {
    let image = asset(80, 50, [200, 70, 40]);
    let request = |video, count, author: &str| {
        json!({"preview":{"bot":"pokopa_bot","blocks":[
            {"kind":"author","text":author},
            {"kind":"media","image":image,"video":video,"count":count},
            {"kind":"caption","text":"Name on X"}
        ]}})
    };
    let plain = render_request(request(false, 1, "Name on X"));
    let video = render_request(request(true, 1, "Name on X"));
    let carousel = render_request(request(false, 3, "Name on X"));
    let other_author = render_request(request(false, 1, "Other on X"));
    let h = height(&plain);
    assert_eq!(&plain.stdout[..2], &[0xff, 0xd8]);
    let img = image::load_from_memory(&plain.stdout).unwrap().to_rgb8();
    assert_eq!(img.width(), 768);
    assert!(h < 900);
    let video_img = image::load_from_memory(&video.stdout).unwrap().to_rgb8();
    let carousel_img = image::load_from_memory(&carousel.stdout).unwrap().to_rgb8();
    let red = |p: &Rgb<u8>| p[0] > 170 && p[1] < 100 && p[2] < 80;
    let y = (0..h).find(|&y| red(img.get_pixel(384, y))).unwrap();
    let bottom = (y..h).rfind(|&y| red(img.get_pixel(384, y))).unwrap();
    let middle = (y + bottom) / 2;
    assert!(red(img.get_pixel(384, middle)));
    assert!(
        !red(video_img.get_pixel(384, middle)),
        "play overlay is visible"
    );
    assert!(red(img.get_pixel(660, middle)));
    assert!(
        !red(carousel_img.get_pixel(660, middle)),
        "carousel arrow is visible"
    );
    assert_ne!(plain.stdout, other_author.stdout);
}

#[test]
fn cli_renders_preview_unicode_quote_and_limits_text() {
    let render = |blocks| render_request(json!({"preview":{"bot":"","blocks":blocks}}));
    let text = render(json!([{"kind":"text","text":"Hello 世界 👋"}]));
    let quote = render(
        json!([{"kind":"text","text":"Hello 世界 👋"},{"kind":"quote","text":"Привет мир\n日本語の引用"}]),
    );
    assert!(height(&quote) > height(&text));
    assert_ne!(text.stdout, quote.stdout);
    let long = render(json!([{"kind":"text","text":"long text ".repeat(10000)}]));
    assert!(height(&long) < 2500);
    let tall =
        render(json!([{"kind":"media","image":asset(10,200,[20,200,40]),"video":false,"count":1}]));
    assert!(height(&tall) < 1000);
}
