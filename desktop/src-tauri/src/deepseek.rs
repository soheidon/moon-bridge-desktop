use crate::moonbridge_api::{ApiClient, ApiError};
use serde::{Deserialize, Serialize};
use serde_json::{json, Map, Value};

pub const PROVIDER_ID: &str = "deepseek";
pub const ROUTE_ID: &str = "moonbridge";
pub const PRO_MODEL: &str = "deepseek-v4-pro";
pub const FLASH_MODEL: &str = "deepseek-v4-flash";
pub const BASE_URL: &str = "https://api.deepseek.com/anthropic";
pub const PROTOCOL: &str = "anthropic";
pub const VERSION: &str = "2023-06-01";
pub const USER_AGENT: &str = "moonbridge-desktop/0.1";
pub const LOW_REASONING: &str = "low";
pub const HIGH_REASONING: &str = "high";
pub const MAX_REASONING: &str = "max";
const LEGACY_XHIGH_REASONING: &str = "xhigh";

// DeepSeek's thinking mode is intentionally always enabled for the Codex
// workflow in this MVP. Reasoning effort is a separate setting within that
// mode; a future Thinking on/off control must not be conflated with effort.

pub fn allowed_reasoning_efforts(model: &str) -> &'static [&'static str] {
    match model {
        PRO_MODEL => &[HIGH_REASONING, MAX_REASONING],
        FLASH_MODEL => &[LOW_REASONING, HIGH_REASONING, MAX_REASONING],
        _ => &[],
    }
}

fn normalize_reasoning_effort(effort: &str) -> &str {
    if effort == LEGACY_XHIGH_REASONING {
        MAX_REASONING
    } else {
        effort
    }
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct DeepSeekMetadata {
    pub models: Vec<DeepSeekModelMetadata>,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct DeepSeekModelMetadata {
    pub id: String,
    pub display_name: String,
    pub allowed_reasoning_efforts: Vec<String>,
    pub default_reasoning_effort: String,
}

pub fn metadata() -> DeepSeekMetadata {
    DeepSeekMetadata {
        models: [PRO_MODEL, FLASH_MODEL]
            .into_iter()
            .map(|model| DeepSeekModelMetadata {
                id: model.to_string(),
                display_name: model_display_name(model).to_string(),
                allowed_reasoning_efforts: allowed_reasoning_efforts(model)
                    .iter()
                    .map(|effort| (*effort).to_string())
                    .collect(),
                default_reasoning_effort: HIGH_REASONING.to_string(),
            })
            .collect(),
    }
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct DeepSeekStatus {
    pub gateway_running: bool,
    pub provider_exists: bool,
    pub api_key_set: bool,
    pub configured: bool,
    pub active: bool,
    pub selected_model: Option<String>,
    pub reasoning_effort: String,
    pub reasoning_explicitly_configured: bool,
    pub allowed_reasoning_efforts: Vec<String>,
    pub route_alias: String,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ConfigureInput {
    pub api_key: Option<String>,
    pub model: String,
    pub reasoning_effort: String,
}

#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ConnectionTestResult {
    pub ok: bool,
    pub code: String,
    pub message: String,
    pub model: String,
}

#[derive(Clone, Debug, Deserialize)]
struct ConfigGraph {
    revision: String,
    resources: Vec<ConfigResource>,
}

#[derive(Clone, Debug, Deserialize)]
struct ConfigResource {
    kind: String,
    id: String,
    value: Value,
}

#[derive(Clone, Debug, Deserialize)]
struct GraphResponse {
    graph: Option<ConfigGraph>,
}

pub async fn status(api: &ApiClient) -> Result<DeepSeekStatus, ApiError> {
    let graph: ConfigGraph = api.get("/config/graph").await?;
    Ok(status_from_graph(&graph))
}

pub async fn configure(api: &ApiClient, input: ConfigureInput) -> Result<DeepSeekStatus, ApiError> {
    validate_input(&input)?;

    for _ in 0..3 {
        let graph: ConfigGraph = api.get("/config/graph").await?;
        if !has_api_key(&graph) && input.api_key.as_deref().unwrap_or("").trim().is_empty() {
            return Err(ApiError::local(
                "DeepSeek API key is required for first-time setup",
            ));
        }
        match reconcile(api, graph, &input).await {
            Ok(_) => {
                let final_graph: ConfigGraph = api.get("/config/graph").await?;
                verify_final_graph(&final_graph, &input)?;
                return Ok(status_from_graph(&final_graph));
            }
            Err(error) if error.is_revision_conflict() => continue,
            Err(error) => return Err(error),
        }
    }
    Err(ApiError::local(
        "configuration changed repeatedly; retry DeepSeek setup",
    ))
}

pub async fn test_connection(api: &ApiClient) -> Result<ConnectionTestResult, ApiError> {
    let graph: ConfigGraph = api.get("/config/graph").await?;
    let provider = resource(&graph, "provider", PROVIDER_ID)
        .ok_or_else(|| ApiError::local("DeepSeek is not configured"))?;
    let api_key = provider
        .value
        .get("api_key")
        .and_then(Value::as_str)
        .filter(|key| !key.trim().is_empty())
        .ok_or_else(|| ApiError::local("DeepSeek API key is not configured"))?;
    let base_url = provider
        .value
        .get("base_url")
        .and_then(Value::as_str)
        .ok_or_else(|| ApiError::local("DeepSeek base URL is not configured"))?;
    let route = resource(&graph, "route", ROUTE_ID)
        .ok_or_else(|| ApiError::local("DeepSeek route is not configured"))?;
    let model = route
        .value
        .get("model")
        .and_then(Value::as_str)
        .filter(|model| allowed_reasoning_efforts(model).len() > 0)
        .ok_or_else(|| ApiError::local("DeepSeek route model is not configured"))?;
    let offer = resource(&graph, "provider_offer", &format!("{PROVIDER_ID}/{model}"))
        .ok_or_else(|| ApiError::local("DeepSeek route offer is not configured"))?;
    let upstream_model = offer
        .value
        .get("upstream_name")
        .and_then(Value::as_str)
        .filter(|model| !model.is_empty())
        .ok_or_else(|| ApiError::local("DeepSeek upstream model is not configured"))?;

    let response = reqwest::Client::new()
        .post(format!("{}/v1/messages", base_url.trim_end_matches('/')))
        .header("x-api-key", api_key)
        .header("anthropic-version", VERSION)
        .json(&json!({
            "model": upstream_model,
            "max_tokens": 1,
            "messages": [{"role": "user", "content": "ping"}],
        }))
        .send()
        .await
        .map_err(|error| {
            ApiError::local(format!("DeepSeek connection test network failure: {error}"))
        })?;
    let status = response.status();
    let body = response.text().await.map_err(|error| {
        ApiError::local(format!(
            "DeepSeek connection test response failure: {error}"
        ))
    })?;
    if !status.is_success() {
        let (code, message) = classify_connection_status(status.as_u16());
        return Ok(ConnectionTestResult {
            ok: false,
            code: code.to_string(),
            message: message.to_string(),
            model: upstream_model.to_string(),
        });
    }
    let payload: Value = match serde_json::from_str(&body) {
        Ok(payload) => payload,
        Err(_) => {
            return Ok(ConnectionTestResult {
                ok: false,
                code: "invalid_response".to_string(),
                message: "DeepSeek returned an invalid connection-test response".to_string(),
                model: upstream_model.to_string(),
            });
        }
    };
    if !payload.is_object() {
        return Ok(ConnectionTestResult {
            ok: false,
            code: "invalid_response".to_string(),
            message: "DeepSeek returned an invalid connection-test response".to_string(),
            model: upstream_model.to_string(),
        });
    }
    Ok(ConnectionTestResult {
        ok: true,
        code: "ok".to_string(),
        message: "DeepSeek connection succeeded".to_string(),
        model: upstream_model.to_string(),
    })
}

fn classify_connection_status(status: u16) -> (&'static str, &'static str) {
    match status {
        401 | 403 => ("authentication_failed", "DeepSeek authentication failed"),
        404 => ("model_not_found", "DeepSeek model was not found"),
        429 => ("rate_limited", "DeepSeek rate limit reached"),
        500..=599 => ("provider_unavailable", "DeepSeek provider is unavailable"),
        _ => ("provider_error", "DeepSeek rejected the connection test"),
    }
}

async fn reconcile(
    api: &ApiClient,
    mut graph: ConfigGraph,
    input: &ConfigureInput,
) -> Result<DeepSeekStatus, ApiError> {
    for model in [PRO_MODEL, FLASH_MODEL] {
        if resource(&graph, "model", model).is_none() {
            let effort = if model == input.model {
                input.reasoning_effort.as_str()
            } else {
                HIGH_REASONING
            };
            graph = create_resource(api, graph, "model", model, model_value(model, effort)).await?;
        }
    }

    let provider_value = provider_value(&graph, input.api_key.as_deref());
    if resource(&graph, "provider", PROVIDER_ID).is_none() {
        graph = create_resource(api, graph, "provider", PROVIDER_ID, provider_value).await?;
    } else {
        let mut changes = Vec::new();
        let current = resource(&graph, "provider", PROVIDER_ID).expect("provider exists");
        if current.value.get("base_url").and_then(Value::as_str) != Some(BASE_URL) {
            changes.push(
                json!({"kind":"provider","id":PROVIDER_ID,"field":"base_url","value":BASE_URL}),
            );
        }
        if current.value.get("protocol").and_then(Value::as_str) != Some(PROTOCOL) {
            changes.push(
                json!({"kind":"provider","id":PROVIDER_ID,"field":"protocol","value":PROTOCOL}),
            );
        }
        if current.value.get("version").and_then(Value::as_str) != Some(VERSION) {
            changes.push(
                json!({"kind":"provider","id":PROVIDER_ID,"field":"version","value":VERSION}),
            );
        }
        if current.value.get("user_agent").and_then(Value::as_str) != Some(USER_AGENT) {
            changes.push(
                json!({"kind":"provider","id":PROVIDER_ID,"field":"user_agent","value":USER_AGENT}),
            );
        }
        let mut extensions = object_value(current.value.get("extensions"));
        extensions.insert("deepseek_v4".to_string(), json!({"enabled": true}));
        if current.value.get("extensions") != Some(&Value::Object(extensions.clone())) {
            changes.push(
                json!({"kind":"provider","id":PROVIDER_ID,"field":"extensions","value":extensions}),
            );
        }
        if let Some(key) = input
            .api_key
            .as_deref()
            .filter(|key| !key.trim().is_empty())
        {
            changes.push(json!({"kind":"provider","id":PROVIDER_ID,"field":"api_key","value":key}));
        }
        if !changes.is_empty() {
            graph = patch_graph(api, graph, changes).await?;
        }
    }

    let current_reasoning = resource(&graph, "model", &input.model)
        .and_then(|item| item.value.get("default_reasoning_level"))
        .and_then(Value::as_str);
    if current_reasoning != Some(input.reasoning_effort.as_str()) {
        graph = patch_graph(
            api,
            graph,
            vec![json!({
                "kind":"model",
                "id":input.model,
                "field":"default_reasoning_level",
                "value":input.reasoning_effort
            })],
        )
        .await?;
    }

    for (model, input_price, output_price, cache_read) in
        [(PRO_MODEL, 2.0, 8.0, 0.2), (FLASH_MODEL, 1.0, 2.0, 0.02)]
    {
        let id = format!("{PROVIDER_ID}/{model}");
        let value = offer_value(model, input_price, output_price, cache_read);
        if resource(&graph, "provider_offer", &id).is_none() {
            graph = create_resource(api, graph, "provider_offer", &id, value).await?;
        } else {
            let current = resource(&graph, "provider_offer", &id).expect("offer exists");
            let mut changes = Vec::new();
            if current.value.get("model").and_then(Value::as_str) != Some(model) {
                changes
                    .push(json!({"kind":"provider_offer","id":id,"field":"model","value":model}));
            }
            if current.value.get("upstream_name").and_then(Value::as_str) != Some(model) {
                changes.push(
                    json!({"kind":"provider_offer","id":id,"field":"upstream_name","value":model}),
                );
            }
            if current.value.get("priority").and_then(Value::as_i64) != Some(0) {
                changes.push(json!({"kind":"provider_offer","id":id,"field":"priority","value":0}));
            }
            if current.value.get("pricing") != value.get("pricing") {
                changes.push(json!({"kind":"provider_offer","id":id,"field":"pricing","value":value["pricing"]}));
            }
            if !changes.is_empty() {
                graph = patch_graph(api, graph, changes).await?;
            }
        }
    }

    let route = resource(&graph, "route", ROUTE_ID);
    if route.is_none() {
        graph = create_resource(
            api,
            graph,
            "route",
            ROUTE_ID,
            json!({
                "model": input.model,
                "provider": PROVIDER_ID,
                "display_name": "Moon Bridge",
            }),
        )
        .await?;
    } else {
        let route = route.expect("route exists");
        let mut changes = Vec::new();
        if route.value.get("model").and_then(Value::as_str) != Some(input.model.as_str()) {
            changes.push(json!({"kind":"route","id":ROUTE_ID,"field":"model","value":input.model}));
        }
        if route.value.get("provider").and_then(Value::as_str) != Some(PROVIDER_ID) {
            changes
                .push(json!({"kind":"route","id":ROUTE_ID,"field":"provider","value":PROVIDER_ID}));
        }
        if !changes.is_empty() {
            graph = patch_graph(api, graph, changes).await?;
        }
    }

    Ok(status_from_graph(&graph))
}

fn status_from_graph(graph: &ConfigGraph) -> DeepSeekStatus {
    let provider = resource(graph, "provider", PROVIDER_ID);
    let route = resource(graph, "route", ROUTE_ID);
    let selected_model = route
        .and_then(|route| route.value.get("model"))
        .and_then(Value::as_str)
        .map(str::to_string);
    let provider_exists = provider.is_some();
    let api_key_set = provider
        .and_then(|item| item.value.get("api_key"))
        .and_then(Value::as_str)
        .map(|key| !key.is_empty())
        .unwrap_or(false);
    let offers_ready = [PRO_MODEL, FLASH_MODEL].iter().all(|model| {
        resource(graph, "provider_offer", &format!("{PROVIDER_ID}/{model}")).is_some()
    });
    let models_ready = [PRO_MODEL, FLASH_MODEL]
        .iter()
        .all(|model| resource(graph, "model", model).is_some());
    let reasoning_effort = route
        .and_then(|route| route.value.get("model"))
        .and_then(Value::as_str)
        .and_then(|model| resource(graph, "model", model))
        .and_then(|model| model.value.get("default_reasoning_level"))
        .and_then(Value::as_str)
        .map(normalize_reasoning_effort)
        .unwrap_or(HIGH_REASONING)
        .to_string();
    let active = route
        .and_then(|route| route.value.get("provider"))
        .and_then(Value::as_str)
        == Some(PROVIDER_ID)
        && selected_model
            .as_deref()
            .map(|model| model == PRO_MODEL || model == FLASH_MODEL)
            .unwrap_or(false);
    let allowed_reasoning_efforts = selected_model
        .as_deref()
        .map(allowed_reasoning_efforts)
        .unwrap_or(&[])
        .iter()
        .map(|effort| (*effort).to_string())
        .collect();
    let reasoning_explicitly_configured = route
        .and_then(|route| route.value.get("model"))
        .and_then(Value::as_str)
        .and_then(|model| resource(graph, "model", model))
        .and_then(|model| model.value.get("default_reasoning_level"))
        .and_then(Value::as_str)
        .is_some();
    DeepSeekStatus {
        gateway_running: true,
        provider_exists,
        api_key_set,
        configured: provider_exists && api_key_set && models_ready && offers_ready && active,
        active,
        selected_model,
        reasoning_effort,
        reasoning_explicitly_configured,
        allowed_reasoning_efforts,
        route_alias: ROUTE_ID.to_string(),
    }
}

fn has_api_key(graph: &ConfigGraph) -> bool {
    resource(graph, "provider", PROVIDER_ID)
        .and_then(|item| item.value.get("api_key"))
        .and_then(Value::as_str)
        .map(|key| !key.is_empty())
        .unwrap_or(false)
}

async fn create_resource(
    api: &ApiClient,
    graph: ConfigGraph,
    kind: &str,
    id: &str,
    value: Value,
) -> Result<ConfigGraph, ApiError> {
    let response: GraphResponse = api
        .post(
            &format!("/config/resources/{kind}"),
            json!({"baseRevision":graph.revision,"id":id,"value":value}),
        )
        .await?;
    response
        .graph
        .ok_or_else(|| ApiError::local("Moon Bridge did not return an updated config graph"))
}

async fn patch_graph(
    api: &ApiClient,
    graph: ConfigGraph,
    changes: Vec<Value>,
) -> Result<ConfigGraph, ApiError> {
    let response: GraphResponse = api
        .patch(
            "/config/graph",
            json!({"baseRevision":graph.revision,"changes":changes}),
        )
        .await?;
    response
        .graph
        .ok_or_else(|| ApiError::local("Moon Bridge did not return an updated config graph"))
}

fn resource<'a>(graph: &'a ConfigGraph, kind: &str, id: &str) -> Option<&'a ConfigResource> {
    graph
        .resources
        .iter()
        .find(|resource| resource.kind == kind && resource.id == id)
}

fn object_value(value: Option<&Value>) -> Map<String, Value> {
    value
        .and_then(Value::as_object)
        .cloned()
        .unwrap_or_default()
}

fn provider_value(graph: &ConfigGraph, api_key: Option<&str>) -> Value {
    let mut extensions = resource(graph, "provider", PROVIDER_ID)
        .map(|item| object_value(item.value.get("extensions")))
        .unwrap_or_default();
    extensions.insert("deepseek_v4".to_string(), json!({"enabled": true}));
    json!({"base_url":BASE_URL,"api_key":api_key.unwrap_or(""),"version":VERSION,"protocol":PROTOCOL,"user_agent":USER_AGENT,"extensions":extensions})
}

fn model_display_name(model: &str) -> &'static str {
    if model == PRO_MODEL {
        "DeepSeek V4 Pro"
    } else {
        "DeepSeek V4 Flash"
    }
}

fn model_value(model: &str, reasoning_effort: &str) -> Value {
    let display_name = model_display_name(model);
    let supported_reasoning_levels: Vec<Value> = allowed_reasoning_efforts(model)
        .iter()
        .map(|effort| {
            json!({
                "effort": effort,
                "description": match *effort {
                    LOW_REASONING => "Low reasoning effort",
                    HIGH_REASONING => "High reasoning effort",
                    MAX_REASONING => "Maximum reasoning effort",
                    _ => "Reasoning effort",
                },
            })
        })
        .collect();
    json!({"context_window":1000000,"max_output_tokens":384000,"display_name":display_name,"description":"DeepSeek V4 with model-specific low/high/max reasoning effort.","supports_reasoning":true,"default_reasoning_level":normalize_reasoning_effort(reasoning_effort),"supported_reasoning_levels":supported_reasoning_levels,"supports_reasoning_summaries":true,"default_reasoning_summary":"auto","input_modalities":["text"]})
}

fn offer_value(model: &str, input: f64, output: f64, cache_read: f64) -> Value {
    json!({"model":model,"upstream_name":model,"priority":0,"pricing":{"input_price":input,"output_price":output,"cache_write_price":1.0,"cache_read_price":cache_read}})
}

pub fn validate_input(input: &ConfigureInput) -> Result<(), ApiError> {
    if allowed_reasoning_efforts(&input.model).is_empty() {
        return Err(ApiError::local("unsupported DeepSeek model"));
    }
    if !allowed_reasoning_efforts(&input.model).contains(&input.reasoning_effort.as_str()) {
        return Err(ApiError::local("unsupported DeepSeek reasoning effort"));
    }
    if input
        .api_key
        .as_deref()
        .is_some_and(|key| key.trim().is_empty())
    {
        return Err(ApiError::local("DeepSeek API key must not be empty"));
    }
    Ok(())
}

fn number_equal(actual: Option<&Value>, expected: Option<&Value>) -> bool {
    actual.and_then(Value::as_f64) == expected.and_then(Value::as_f64)
}

fn pricing_matches(actual: Option<&Value>, expected: Option<&Value>) -> bool {
    let (Some(actual), Some(expected)) = (
        actual.and_then(Value::as_object),
        expected.and_then(Value::as_object),
    ) else {
        return false;
    };
    [
        "input_price",
        "output_price",
        "cache_write_price",
        "cache_read_price",
    ]
    .iter()
    .all(|field| number_equal(actual.get(*field), expected.get(*field)))
}

fn verify_final_graph(graph: &ConfigGraph, input: &ConfigureInput) -> Result<(), ApiError> {
    let provider = resource(graph, "provider", PROVIDER_ID)
        .ok_or_else(|| ApiError::local("final_state_mismatch: DeepSeek provider"))?;
    for (field, expected) in [
        ("base_url", BASE_URL),
        ("protocol", PROTOCOL),
        ("version", VERSION),
        ("user_agent", USER_AGENT),
    ] {
        if provider.value.get(field).and_then(Value::as_str) != Some(expected) {
            return Err(ApiError::local(format!(
                "final_state_mismatch: provider {field}"
            )));
        }
    }
    if provider
        .value
        .get("api_key")
        .and_then(Value::as_str)
        .is_none_or(str::is_empty)
    {
        return Err(ApiError::local("final_state_mismatch: provider api_key"));
    }
    let extension_enabled = provider
        .value
        .get("extensions")
        .and_then(|value| value.get("deepseek_v4"))
        .and_then(|value| value.get("enabled"))
        .and_then(Value::as_bool)
        == Some(true);
    if !extension_enabled {
        return Err(ApiError::local(
            "final_state_mismatch: deepseek_v4 extension",
        ));
    }

    for (model, input_price, output_price, cache_read) in
        [(PRO_MODEL, 2.0, 8.0, 0.2), (FLASH_MODEL, 1.0, 2.0, 0.02)]
    {
        if resource(graph, "model", model).is_none() {
            return Err(ApiError::local(format!(
                "final_state_mismatch: model {model}"
            )));
        }
        let offer_id = format!("{PROVIDER_ID}/{model}");
        let offer = resource(graph, "provider_offer", &offer_id)
            .ok_or_else(|| ApiError::local(format!("final_state_mismatch: offer {offer_id}")))?;
        let expected = offer_value(model, input_price, output_price, cache_read);
        if offer.value.get("model") != expected.get("model")
            || offer.value.get("upstream_name") != expected.get("upstream_name")
            || offer.value.get("priority") != expected.get("priority")
            || !pricing_matches(offer.value.get("pricing"), expected.get("pricing"))
        {
            return Err(ApiError::local(format!(
                "final_state_mismatch: offer {offer_id}"
            )));
        }
    }

    let route = resource(graph, "route", ROUTE_ID)
        .ok_or_else(|| ApiError::local("final_state_mismatch: moonbridge route"))?;
    if route.value.get("provider").and_then(Value::as_str) != Some(PROVIDER_ID)
        || route.value.get("model").and_then(Value::as_str) != Some(input.model.as_str())
    {
        return Err(ApiError::local(
            "final_state_mismatch: moonbridge route target",
        ));
    }
    let model = resource(graph, "model", &input.model)
        .ok_or_else(|| ApiError::local("final_state_mismatch: selected model"))?;
    if model
        .value
        .get("default_reasoning_level")
        .and_then(Value::as_str)
        != Some(input.reasoning_effort.as_str())
    {
        return Err(ApiError::local("final_state_mismatch: selected reasoning"));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn model_values_match_supported_deepseek_models() {
        assert_eq!(
            model_value(PRO_MODEL, HIGH_REASONING)["context_window"],
            1000000
        );
        assert_eq!(
            model_value(FLASH_MODEL, MAX_REASONING)["max_output_tokens"],
            384000
        );
        assert_eq!(
            model_value(FLASH_MODEL, MAX_REASONING)["default_reasoning_level"],
            MAX_REASONING
        );
        assert_eq!(
            model_value(PRO_MODEL, MAX_REASONING)["supported_reasoning_levels"],
            json!([
                {"effort": HIGH_REASONING, "description": "High reasoning effort"},
                {"effort": MAX_REASONING, "description": "Maximum reasoning effort"}
            ])
        );
        assert_eq!(
            model_value(FLASH_MODEL, LOW_REASONING)["supported_reasoning_levels"],
            json!([
                {"effort": LOW_REASONING, "description": "Low reasoning effort"},
                {"effort": HIGH_REASONING, "description": "High reasoning effort"},
                {"effort": MAX_REASONING, "description": "Maximum reasoning effort"}
            ])
        );
    }

    #[test]
    fn provider_value_enables_deepseek_extension() {
        let graph = ConfigGraph {
            revision: "r1".into(),
            resources: vec![],
        };
        assert_eq!(
            provider_value(&graph, Some("secret"))["extensions"]["deepseek_v4"]["enabled"],
            true
        );
    }

    #[test]
    fn metadata_exposes_both_models_and_reasoning_options() {
        let value = metadata();
        assert_eq!(value.models.len(), 2);
        assert_eq!(
            value.models[0].allowed_reasoning_efforts,
            [HIGH_REASONING, MAX_REASONING]
        );
        assert_eq!(
            value.models[1].allowed_reasoning_efforts,
            [LOW_REASONING, HIGH_REASONING, MAX_REASONING]
        );
    }

    #[test]
    fn input_validation_rejects_empty_keys_before_mutation() {
        let input = ConfigureInput {
            api_key: Some("  ".to_string()),
            model: PRO_MODEL.to_string(),
            reasoning_effort: HIGH_REASONING.to_string(),
        };
        assert_eq!(
            validate_input(&input).unwrap_err().message,
            "DeepSeek API key must not be empty"
        );
    }

    #[test]
    fn input_validation_rejects_unsupported_model_and_effort() {
        let unsupported_model = ConfigureInput {
            api_key: None,
            model: "deepseek-v3".to_string(),
            reasoning_effort: HIGH_REASONING.to_string(),
        };
        assert!(validate_input(&unsupported_model).is_err());

        let unsupported_effort = ConfigureInput {
            api_key: None,
            model: PRO_MODEL.to_string(),
            reasoning_effort: LOW_REASONING.to_string(),
        };
        assert!(validate_input(&unsupported_effort).is_err());

        let flash_low = ConfigureInput {
            api_key: None,
            model: FLASH_MODEL.to_string(),
            reasoning_effort: LOW_REASONING.to_string(),
        };
        assert!(validate_input(&flash_low).is_ok());
    }

    #[test]
    fn legacy_xhigh_is_read_as_official_max_value() {
        assert_eq!(
            normalize_reasoning_effort(LEGACY_XHIGH_REASONING),
            MAX_REASONING
        );
        assert_eq!(normalize_reasoning_effort(HIGH_REASONING), HIGH_REASONING);
    }

    fn complete_graph(input: &ConfigureInput) -> ConfigGraph {
        ConfigGraph {
            revision: "r2".to_string(),
            resources: vec![
                ConfigResource {
                    kind: "provider".to_string(),
                    id: PROVIDER_ID.to_string(),
                    value: provider_value(
                        &ConfigGraph {
                            revision: "r1".to_string(),
                            resources: vec![],
                        },
                        Some("masked-secret"),
                    ),
                },
                ConfigResource {
                    kind: "model".to_string(),
                    id: PRO_MODEL.to_string(),
                    value: model_value(
                        PRO_MODEL,
                        if input.model == PRO_MODEL {
                            &input.reasoning_effort
                        } else {
                            HIGH_REASONING
                        },
                    ),
                },
                ConfigResource {
                    kind: "model".to_string(),
                    id: FLASH_MODEL.to_string(),
                    value: model_value(
                        FLASH_MODEL,
                        if input.model == FLASH_MODEL {
                            &input.reasoning_effort
                        } else {
                            HIGH_REASONING
                        },
                    ),
                },
                ConfigResource {
                    kind: "provider_offer".to_string(),
                    id: format!("{PROVIDER_ID}/{PRO_MODEL}"),
                    value: offer_value(PRO_MODEL, 2.0, 8.0, 0.2),
                },
                ConfigResource {
                    kind: "provider_offer".to_string(),
                    id: format!("{PROVIDER_ID}/{FLASH_MODEL}"),
                    value: offer_value(FLASH_MODEL, 1.0, 2.0, 0.02),
                },
                ConfigResource {
                    kind: "route".to_string(),
                    id: ROUTE_ID.to_string(),
                    value: json!({
                        "model": input.model,
                        "provider": PROVIDER_ID,
                        "display_name": "Moon Bridge",
                    }),
                },
            ],
        }
    }

    #[test]
    fn final_graph_verification_accepts_complete_graph() {
        let input = ConfigureInput {
            api_key: None,
            model: FLASH_MODEL.to_string(),
            reasoning_effort: MAX_REASONING.to_string(),
        };
        assert!(verify_final_graph(&complete_graph(&input), &input).is_ok());
    }

    #[test]
    fn final_graph_verification_rejects_partial_state() {
        let input = ConfigureInput {
            api_key: None,
            model: PRO_MODEL.to_string(),
            reasoning_effort: HIGH_REASONING.to_string(),
        };
        let mut graph = complete_graph(&input);
        graph.resources.retain(|resource| {
            !(resource.kind == "provider_offer" && resource.id.ends_with(FLASH_MODEL))
        });
        let error = verify_final_graph(&graph, &input).unwrap_err();
        assert!(error.message.contains("final_state_mismatch"));
    }

    #[test]
    fn connection_status_classification_keeps_rate_limit_distinct() {
        assert_eq!(classify_connection_status(429).0, "rate_limited");
        assert_eq!(classify_connection_status(401).0, "authentication_failed");
    }
}
