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
