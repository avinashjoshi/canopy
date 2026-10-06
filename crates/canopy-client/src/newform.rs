//! The "new workspace" form shared by the dashboard and the sidebar: Fresh / PR / Issue /
//! Branch sources, with lists fetched through the server (so it works over `--remote`).

use crate::Transport;
use canopy_proto::{Method, PickItem, ResultBody, WorkspaceCreate};
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use std::path::PathBuf;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Source {
    Fresh,
    Pr,
    Issue,
    Branch,
}

impl Source {
    pub const ALL: [Source; 4] = [Source::Fresh, Source::Pr, Source::Issue, Source::Branch];
    pub fn label(self) -> &'static str {
        match self {
            Source::Fresh => "fresh",
            Source::Pr => "PR",
            Source::Issue => "issue",
            Source::Branch => "branch",
        }
    }
    fn next(self) -> Self {
        let i = Self::ALL.iter().position(|s| *s == self).unwrap_or(0);
        Self::ALL[(i + 1) % Self::ALL.len()]
    }
    fn prev(self) -> Self {
        let i = Self::ALL.iter().position(|s| *s == self).unwrap_or(0);
        Self::ALL[(i + Self::ALL.len() - 1) % Self::ALL.len()]
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NewForm {
    pub project_root: PathBuf,
    pub project_name: String,
    pub source: Source,
    /// Fresh: 0 name, 1 prompt, 2 agent.
    pub field: usize,
    pub name: String,
    pub prompt: String,
    pub agent: String,
    /// Picker state for PR/issue/branch.
    pub filter: String,
    pub items: Vec<PickItem>,
    pub loaded: Option<Source>,
    pub loading: bool,
    pub error: String,
    pub sel: usize,
}

pub enum Action {
    None,
    Cancel,
    /// Fetch items for this source (caller runs `load` in the background).
    Load(Source),
    Submit(WorkspaceCreate),
}

impl NewForm {
    pub fn new(project_root: PathBuf, project_name: String) -> Self {
        Self {
            project_root,
            project_name,
            source: Source::Fresh,
            field: 0,
            name: String::new(),
            prompt: String::new(),
            agent: String::new(),
            filter: String::new(),
            items: Vec::new(),
            loaded: None,
            loading: false,
            error: String::new(),
            sel: 0,
        }
    }

    pub fn visible(&self) -> Vec<&PickItem> {
        let f = self.filter.to_lowercase();
        self.items.iter().filter(|i| f.is_empty() || format!("{} {}", i.label, i.detail).to_lowercase().contains(&f)).collect()
    }

    pub fn set_items(&mut self, source: Source, items: Result<Vec<PickItem>, String>) {
        if source != self.source {
            return;
        }
        self.loading = false;
        match items {
            Ok(v) => {
                self.items = v;
                self.loaded = Some(source);
                self.error.clear();
            }
            Err(e) => self.error = e,
        }
        self.sel = 0;
    }

    fn switch(&mut self, to: Source) -> Action {
        self.source = to;
        self.filter.clear();
        self.sel = 0;
        if to == Source::Fresh || self.loaded == Some(to) {
            Action::None
        } else {
            self.items.clear();
            self.loading = true;
            self.error.clear();
            Action::Load(to)
        }
    }

    pub fn handle_key(&mut self, key: KeyEvent) -> Action {
        let ctrl = key.modifiers.contains(KeyModifiers::CONTROL);
        match key.code {
            KeyCode::Esc => return Action::Cancel,
            KeyCode::Char('c') if ctrl => return Action::Cancel,
            KeyCode::Left if self.source != Source::Fresh || self.field == 0 => return self.switch(self.source.prev()),
            KeyCode::Right if self.source != Source::Fresh || self.field == 0 => return self.switch(self.source.next()),
            KeyCode::Char('1') if ctrl => return self.switch(Source::Fresh),
            KeyCode::Char('2') if ctrl => return self.switch(Source::Pr),
            KeyCode::Char('3') if ctrl => return self.switch(Source::Issue),
            KeyCode::Char('4') if ctrl => return self.switch(Source::Branch),
            KeyCode::Tab => return self.switch(self.source.next()),
            KeyCode::BackTab => return self.switch(self.source.prev()),
            _ => {}
        }
        if self.source == Source::Fresh {
            match key.code {
                KeyCode::Down => self.field = (self.field + 1) % 3,
                KeyCode::Up => self.field = (self.field + 2) % 3,
                KeyCode::Backspace => {
                    match self.field {
                        0 => self.name.pop(),
                        1 => self.prompt.pop(),
                        _ => self.agent.pop(),
                    };
                }
                KeyCode::Enter => {
                    return Action::Submit(WorkspaceCreate {
                        project_root: self.project_root.clone(),
                        name: (!self.name.trim().is_empty()).then(|| self.name.trim().to_string()),
                        prompt: (!self.prompt.trim().is_empty()).then(|| self.prompt.trim().to_string()),
                        agent: (!self.agent.trim().is_empty()).then(|| self.agent.trim().to_string()),
                        start_session: Some(true),
                        ..Default::default()
                    });
                }
                KeyCode::Char(c) if !ctrl => match self.field {
                    0 => self.name.push(c),
                    1 => self.prompt.push(c),
                    _ => self.agent.push(c),
                },
                _ => {}
            }
            return Action::None;
        }
        // Picker sources.
        let n = self.visible().len();
        match key.code {
            KeyCode::Down | KeyCode::Char('j') if ctrl || key.code == KeyCode::Down => {
                if n > 0 {
                    self.sel = (self.sel + 1).min(n - 1);
                }
            }
            KeyCode::Up | KeyCode::Char('k') if ctrl || key.code == KeyCode::Up => {
                self.sel = self.sel.saturating_sub(1);
            }
            KeyCode::Backspace => {
                self.filter.pop();
                self.sel = 0;
            }
            KeyCode::Enter => {
                let Some(item) = self.visible().get(self.sel).map(|i| (*i).clone()) else { return Action::None };
                let mut req = WorkspaceCreate { project_root: self.project_root.clone(), start_session: Some(true), ..Default::default() };
                match self.source {
                    Source::Pr => req.pr = item.key.parse().ok(),
                    Source::Issue => req.issue = item.key.parse().ok(),
                    Source::Branch => req.branch = Some(item.key.clone()),
                    Source::Fresh => {}
                }
                return Action::Submit(req);
            }
            KeyCode::Char(c) if !ctrl => {
                self.filter.push(c);
                self.sel = 0;
            }
            _ => {}
        }
        Action::None
    }
}

/// Fetch picker items for a source. Blocking; run on a background thread.
pub fn load(t: &Transport, root: &std::path::Path, source: Source) -> Result<Vec<PickItem>, String> {
    let method = match source {
        Source::Pr => Method::ProjectPullRequests { root: root.to_path_buf() },
        Source::Issue => Method::ProjectIssues { root: root.to_path_buf() },
        Source::Branch => Method::ProjectBranches { root: root.to_path_buf() },
        Source::Fresh => return Ok(Vec::new()),
    };
    match t.call(method) {
        Ok(ResultBody::PickList { items }) => Ok(items),
        Ok(_) => Err("unexpected response".into()),
        Err(e) => Err(e.to_string()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn key(code: KeyCode) -> KeyEvent {
        KeyEvent::new(code, KeyModifiers::NONE)
    }

    #[test]
    fn fresh_submit_collects_fields() {
        let mut f = NewForm::new("/p".into(), "p".into());
        for c in "fix-tz".chars() {
            f.handle_key(key(KeyCode::Char(c)));
        }
        f.handle_key(key(KeyCode::Down));
        for c in "do it".chars() {
            f.handle_key(key(KeyCode::Char(c)));
        }
        let Action::Submit(req) = f.handle_key(key(KeyCode::Enter)) else { panic!() };
        assert_eq!(req.name.as_deref(), Some("fix-tz"));
        assert_eq!(req.prompt.as_deref(), Some("do it"));
        assert!(req.pr.is_none());
    }

    #[test]
    fn switching_to_pr_requests_load_once() {
        let mut f = NewForm::new("/p".into(), "p".into());
        assert!(matches!(f.handle_key(key(KeyCode::Right)), Action::Load(Source::Pr)));
        assert!(f.loading);
        f.set_items(Source::Pr, Ok(vec![PickItem { key: "12".into(), label: "#12 fix".into(), detail: "@a · b".into(), in_use: false }, PickItem { key: "9".into(), label: "#9 other".into(), detail: String::new(), in_use: true }]));
        assert!(!f.loading);
        // filter then pick
        for c in "oth".chars() {
            f.handle_key(key(KeyCode::Char(c)));
        }
        assert_eq!(f.visible().len(), 1);
        let Action::Submit(req) = f.handle_key(key(KeyCode::Enter)) else { panic!() };
        assert_eq!(req.pr, Some(9));
        // back and forth does not reload
        f.handle_key(key(KeyCode::Left));
        assert!(matches!(f.handle_key(key(KeyCode::Right)), Action::None));
    }

    #[test]
    fn tab_cycles_sources_not_fields() {
        let mut f = NewForm::new("/p".into(), "p".into());
        assert!(matches!(f.handle_key(key(KeyCode::Tab)), Action::Load(Source::Pr)));
        assert_eq!(f.source, Source::Pr);
        f.handle_key(key(KeyCode::BackTab));
        assert_eq!(f.source, Source::Fresh);
        assert_eq!(f.field, 0);
    }

    #[test]
    fn stale_results_ignored() {
        let mut f = NewForm::new("/p".into(), "p".into());
        f.handle_key(key(KeyCode::Right)); // PR
        f.handle_key(key(KeyCode::Right)); // issue
        f.set_items(Source::Pr, Ok(vec![PickItem { key: "1".into(), label: "x".into(), detail: String::new(), in_use: false }]));
        assert!(f.items.is_empty());
        assert_eq!(f.source, Source::Issue);
    }
}
