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
fn cli_renders_theme_palettes() {
    for (theme, background) in [
        ("light", [247, 249, 249]),
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
fn cli_light_bars_are_black_only_between_photos() {
    let images =
        json!({"red": asset(10, 40, [220, 30, 30]), "green": asset(10, 40, [30, 220, 30])});
    let photos = json!([{"url":"red"}, {"url":"green"}]);
    for bars in [false, true] {
        let output = render(
            json!({"carousel": true, "bars": bars, "photos": photos}),
            images.clone(),
        );
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
        let gap = image.get_pixel((red_end + green_start) / 2, y);
        if bars {
            assert!(
                gap.0.iter().all(|&channel| channel < 60),
                "bars gap {gap:?}"
            );
            for y in [ys[0] + 1, ys[ys.len() - 1] - 1] {
                let corner = image.get_pixel(green_end - 1, y);
                assert!(
                    corner.0.iter().all(|&channel| channel > 200),
                    "bars corner {corner:?}"
                );
            }
        } else {
            assert!(
                gap.0.iter().all(|&channel| channel > 200),
                "normal gap {gap:?}"
            );
        }
        let beyond = image.get_pixel(green_end + 30, y);
        assert!(
            beyond.0.iter().all(|&channel| channel > 200),
            "after photos {beyond:?}"
        );
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
