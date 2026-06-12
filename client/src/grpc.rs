use anyhow::Result;
use tonic::transport::{Certificate, Channel, ClientTlsConfig};

pub mod client_v1 {
    tonic::include_proto!("client.v1");
}
pub use client_v1::client_service_client::ClientServiceClient;

pub async fn connect_grpc(
    controller_address: &str,
    ca_pem: &str,
) -> Result<ClientServiceClient<Channel>> {
    let tls = ClientTlsConfig::new()
        .ca_certificate(Certificate::from_pem(ca_pem.as_bytes()))
        .domain_name(controller_host(controller_address));
    let channel = Channel::from_shared(format!("https://{}", controller_address))?
        .tls_config(tls)?
        .connect()
        .await?;
    Ok(ClientServiceClient::new(channel))
}

fn controller_host(controller_address: &str) -> String {
    if let Some(rest) = controller_address.strip_prefix('[') {
        if let Some((host, _)) = rest.split_once(']') {
            return host.to_string();
        }
    }

    controller_address
        .rsplit_once(':')
        .map(|(host, _)| host)
        .unwrap_or(controller_address)
        .to_string()
}
