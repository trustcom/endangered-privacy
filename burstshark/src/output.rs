use std::io::{stdout, Write};
use std::time::SystemTime;

use tokio::sync::mpsc;

use crate::capture::Burst;

pub struct OutputWriter {
    min_bytes: Option<u32>,
    max_bytes: Option<u32>,
    min_packets: Option<u16>,
    max_packets: Option<u16>,
}

impl OutputWriter {
    pub fn new(
        min_bytes: Option<u32>,
        max_bytes: Option<u32>,
        min_packets: Option<u16>,
        max_packets: Option<u16>,
    ) -> Self {
        OutputWriter {
            min_bytes,
            max_bytes,
            min_packets,
            max_packets,
        }
    }

    pub async fn start(&mut self) -> mpsc::Sender<Burst> {
        let (tx, mut rx) = mpsc::channel::<Burst>(100);

        let min_bytes = self.min_bytes;
        let max_bytes = self.max_bytes;
        let min_packets = self.min_packets;
        let max_packets = self.max_packets;

        tokio::spawn(async move {
            let stdout = stdout();
            let start_time = SystemTime::now();
            let mut count = 0;

            while let Some(burst) = rx.recv().await {
                if (min_bytes.map_or(false, |min| burst.size < min))
                    || (max_bytes.map_or(false, |max| burst.size > max))
                    || (min_packets.map_or(false, |min| burst.num_packets < min))
                    || (max_packets.map_or(false, |max| burst.num_packets > max))
                {
                    continue;
                }

                count += 1;

                let elapsed = start_time.elapsed().unwrap_or_default().as_secs_f64();
                let delay = SystemTime::UNIX_EPOCH
                    .elapsed()
                    .unwrap_or_default()
                    .as_secs_f64()
                    - burst.end;

                let mut handle = stdout.lock();

                writeln!(
                    &mut handle,
                    "{:5} {:13.9} {:15} {:6} {:15} {:5} {:13.9} {:13.9} {:13.9} {:4} {}",
                    count,
                    elapsed,
                    burst.src,
                    burst.src_port,
                    burst.dst,
                    burst.dst_port,
                    burst.start,
                    burst.end,
                    delay,
                    burst.num_packets,
                    burst.size,
                )
                .unwrap();
            }
        });

        tx
    }
}
