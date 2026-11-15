mod card;

use std::io::{self, Read, Write};

fn main() {
    if let Err(error) = run() {
        eprintln!("kopoka-render: {error:#}");
        std::process::exit(1);
    }
}

fn run() -> anyhow::Result<()> {
    let mut input = Vec::new();
    io::stdin().read_to_end(&mut input)?;
    let request = serde_json::from_slice(&input)?;
    let jpeg = card::render(request)?;
    io::stdout().lock().write_all(&jpeg)?;
    Ok(())
}
