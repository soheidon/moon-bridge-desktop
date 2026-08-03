use reqwest::{Client, Method, StatusCode};
use serde::de::DeserializeOwned;
use serde_json::Value;

#[derive(Debug, Clone)]
pub struct ApiError {
    pub status: Option<u16>,
    pub code: Option<String>,
    pub message: String,
}

impl ApiError {
    pub fn local(message: impl Into<String>) -> Self {
        Self {
            status: None,
            code: None,
            message: message.into(),
        }
    }

    pub fn is_revision_conflict(&self) -> bool {
        self.status == Some(StatusCode::CONFLICT.as_u16())
    }
}

pub struct ApiClient {
    client: Client,
    address: String,
    token: String,
}

impl ApiClient {
    pub fn new(client: Client, address: impl Into<String>, token: impl Into<String>) -> Self {
        Self {
            client,
            address: address.into(),
            token: token.into(),
        }
    }

    pub async fn get<T: DeserializeOwned>(&self, path: &str) -> Result<T, ApiError> {
        self.request(Method::GET, path, None).await
    }

    pub async fn post<T: DeserializeOwned>(&self, path: &str, body: Value) -> Result<T, ApiError> {
        self.request(Method::POST, path, Some(body)).await
    }

    pub async fn patch<T: DeserializeOwned>(&self, path: &str, body: Value) -> Result<T, ApiError> {
        self.request(Method::PATCH, path, Some(body)).await
    }

    async fn request<T: DeserializeOwned>(
        &self,
        method: Method,
        path: &str,
        body: Option<Value>,
    ) -> Result<T, ApiError> {
        let url = format!("http://{}/api/v1{}", self.address, path);
        let mut request = self.client.request(method, url).bearer_auth(&self.token);
        if let Some(body) = body {
            request = request.json(&body);
        }
        let response = request
            .send()
            .await
            .map_err(|err| ApiError::local(format!("Moon Bridge API request failed: {err}")))?;
        let status = response.status();
        let payload = response.text().await.map_err(|err| ApiError {
            status: Some(status.as_u16()),
            code: None,
            message: format!("read Moon Bridge API response failed: {err}"),
        })?;
        if !status.is_success() {
            return Err(ApiError {
                status: Some(status.as_u16()),
                code: api_error_code(&payload),
                message: api_error_message(&payload),
            });
        }
        serde_json::from_str(&payload).map_err(|err| ApiError {
            status: Some(status.as_u16()),
            code: None,
            message: format!("decode Moon Bridge API response failed: {err}"),
        })
    }
}

fn api_error_message(payload: &str) -> String {
    let value: Value = match serde_json::from_str(payload) {
        Ok(value) => value,
        Err(_) => return "Moon Bridge API returned an error".to_string(),
    };
    value
        .get("message")
        .and_then(Value::as_str)
        .or_else(|| {
            value
                .get("error")
                .and_then(|error| error.get("message"))
                .and_then(Value::as_str)
        })
        .or_else(|| {
            value
                .get("errors")
                .and_then(Value::as_array)
                .and_then(|errors| errors.first())
                .and_then(|error| error.get("message"))
                .and_then(Value::as_str)
        })
        .unwrap_or("Moon Bridge API returned an error")
        .to_string()
}

fn api_error_code(payload: &str) -> Option<String> {
    let value: Value = serde_json::from_str(payload).ok()?;
    value
        .get("code")
        .and_then(Value::as_str)
        .or_else(|| {
            value
                .get("error")
                .and_then(|error| error.get("code"))
                .and_then(Value::as_str)
        })
        .map(ToOwned::to_owned)
}

#[cfg(test)]
mod tests {
    use super::{api_error_code, api_error_message};

    #[test]
    fn preserves_flat_code_and_message_from_capture_api() {
        let payload = r#"{"code":"capture_pause_drain_timeout","message":"drain timed out"}"#;
        assert_eq!(
            api_error_code(payload).as_deref(),
            Some("capture_pause_drain_timeout")
        );
        assert_eq!(api_error_message(payload), "drain timed out");
    }
}
