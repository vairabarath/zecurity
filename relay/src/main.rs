mod appmeta;
mod csr;
mod protocol;
mod provision;
mod spiffe;
mod state;

pub mod relay {
    pub mod v1 {
        tonic::include_proto!("relay.v1");
    }
}

fn main() {
    println!("relay");
}
