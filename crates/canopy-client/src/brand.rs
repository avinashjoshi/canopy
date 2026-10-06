//! The canopy mark: one glyph, `ᛉ` by default (`[ui] glyph` in config.toml, or `CANOPY_GLYPH`
//! for a quick look), used in the sidebar header, the collapsed strip and the dashboard pill.

use ratatui::style::{Color, Modifier, Style};
use ratatui::text::{Line, Span};

pub const ACCENT: Color = Color::Rgb(110, 231, 167);

pub fn glyph() -> String {
    static GLYPH: std::sync::OnceLock<String> = std::sync::OnceLock::new();
    GLYPH
        .get_or_init(|| {
            if let Some(g) = std::env::var("CANOPY_GLYPH").ok().filter(|s| !s.trim().is_empty()) {
                return g;
            }
            let paths = canopy_core::paths::Paths::from_env();
            canopy_core::settings::Settings::load(&paths.settings_file()).map(|s| s.ui.glyph).unwrap_or_else(|_| "ᛉ".to_string())
        })
        .clone()
}

/// `ᛉ canopy` as a styled line.
pub fn inline(accent: Color) -> Line<'static> {
    Line::from(vec![Span::styled(format!("{} ", glyph()), Style::new().fg(accent).add_modifier(Modifier::BOLD)), Span::styled("canopy", Style::new().add_modifier(Modifier::BOLD))])
}
