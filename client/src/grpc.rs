use tonic::transport::{Channel, ClientTlsConfig};
use anyhow::Result;

pub mod client_v1 {
    tonic::include_proto!("client.v1");
}
pub use client_v1::client_service_client::ClientServiceClient;

pub async fn connect_grpc(controller_address: &str) -> Result<ClientServiceClient<Channel>> {
    let tls = ClientTlsConfig::new();
    let channel = Channel::from_shared(format!("https://{}", controller_address))?
        .tls_config(tls)?
        .connect()
        .await?;
    Ok(ClientServiceClient::new(channel))
}
