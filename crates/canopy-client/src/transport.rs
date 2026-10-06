//! Where API calls go: the local socket, or a remote host over the ssh bridge.

use crate::{call_raw as local_call_raw, remote, ApiFailure};
use anyhow::Result;
use canopy_proto::{Method, Response, ResultBody};
use std::io::BufRead;
use std::path::PathBuf;
use std::time::Duration;

#[derive(Debug, Clone)]
pub enum Transport {
    Local(PathBuf),
    Ssh { target: String },
}

impl Transport {
    pub fn is_remote(&self) -> bool {
        matches!(self, Transport::Ssh { .. })
    }

    pub fn host_label(&self) -> String {
        match self {
            Transport::Local(_) => String::new(),
            Transport::Ssh { target } => target.rsplit('@').next().unwrap_or(target).to_string(),
        }
    }

    pub fn call_raw(&self, method: Method, timeout: Option<Duration>) -> Result<Response> {
        match self {
            Transport::Local(sock) => local_call_raw(sock, method, timeout),
            Transport::Ssh { target } => remote::call_raw(target, method, true),
        }
    }

    pub fn call(&self, method: Method) -> Result<ResultBody> {
        let slow = matches!(method, Method::WorkspaceCreate(_) | Method::WorkspaceRemove { .. } | Method::WorkspaceRetry { .. } | Method::WorkspaceResurrect { .. } | Method::WorkspaceAttachTarget { .. } | Method::ProjectMain { .. });
        let timeout = if slow { None } else { Some(Duration::from_secs(10)) };
        match self.call_raw(method, timeout)? {
            Response::Ok { result, .. } => Ok(result),
            Response::Err { error, .. } => Err(ApiFailure(error).into()),
        }
    }

    /// Event stream: each line is a `Response`. The returned guard keeps a remote bridge alive.
    pub fn subscribe(&self) -> Result<(Box<dyn BufRead + Send>, Option<std::process::Child>)> {
        match self {
            Transport::Local(sock) => Ok((Box::new(crate::subscribe(sock, None)?), None)),
            Transport::Ssh { target } => {
                let (child, reader) = remote::subscribe(target)?;
                Ok((Box::new(reader), Some(child)))
            }
        }
    }
}
