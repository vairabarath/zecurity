const fn get_controller() -> &'static str {
    match option_env!("ZECURITY_CONTROLLER_ADDRESS") {
        Some(s) => s,
        None => "localhost:9090",
    }
}

const fn get_connector() -> &'static str {
    match option_env!("ZECURITY_CONNECTOR_ADDRESS") {
        Some(s) => s,
        None => "localhost:9092",
    }
}

pub const DEFAULT_CONTROLLER_ADDRESS: &str = get_controller();
pub const DEFAULT_CONNECTOR_ADDRESS: &str = get_connector();
pub const SCHEMA_VERSION: u32 = 1;
