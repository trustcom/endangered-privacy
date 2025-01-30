use std::collections::{hash_map::Entry, HashMap};
use std::error::Error;
use std::process::Stdio;

use nix::sys::signal::{kill, Signal};
use nix::unistd::Pid;
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::process::Command;
use tokio::sync::mpsc;
use tokio::time::{sleep, Duration};

const FLOW_TIMEOUT: f64 = 30.0;

type FlowKey = (String, String, u16, u16);

#[derive(Debug, Clone)]
pub struct Burst {
    pub src: String,
    pub dst: String,
    pub src_port: u16,
    pub dst_port: u16,
    pub start: f64,
    pub end: f64,
    pub num_packets: u16,
    pub size: u32,
}

#[derive(Debug, Clone)]
pub struct CommonOptions {
    pub tshark_args: Vec<String>,
    pub burst_timeout: f64,
    pub output_tx: mpsc::Sender<Burst>,
}

#[derive(Debug, Clone)]
pub enum CaptureType {
    Ip {
        opts: CommonOptions,
        aggregate_ports: bool,
    },
    Wlan {
        opts: CommonOptions,
        no_estimation: bool,
        max_deviation: u16,
    },
}

impl CaptureType {
    pub async fn run(&self) -> Result<(), Box<dyn Error>> {
        let opts = match self {
            CaptureType::Ip { opts, .. } | CaptureType::Wlan { opts, .. } => opts,
        };

        let mut tshark = Command::new("tshark")
            .args(&opts.tshark_args)
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .spawn()
            .map_err(|err| format!("failed to start tshark: {}", err))?;

        if let Some(tshark_pid) = tshark.id() {
            let tshark_pid = tshark_pid as i32;
            tokio::spawn(async move {
                tokio::signal::ctrl_c().await.unwrap();
                kill(Pid::from_raw(tshark_pid), Signal::SIGTERM).unwrap();
            });
        }

        let stdout = tshark.stdout.take().unwrap();
        let reader = BufReader::new(stdout);
        let mut lines = reader.lines();

        let mut flows = HashMap::<FlowKey, mpsc::Sender<Packet>>::new();
        let (timeout_tx, mut timeout_rx) = mpsc::channel::<FlowKey>(100);

        loop {
            tokio::select! {
                line = lines.next_line() => {
                    match line? {
                        Some(line) => {
                            let packet = Packet::from_tshark(&line, self).map_err(|err| {
                                format!("failed to parse packet: {}", err)
                            })?;

                            let flow_key = (
                                packet.src.clone(),
                                packet.dst.clone(),
                                packet.src_port,
                                packet.dst_port,
                            );

                            match flows.entry(flow_key) {
                                Entry::Occupied(mut entry) => {
                                    entry.get_mut().send(packet).await?;
                                },
                                Entry::Vacant(entry) => {
                                    let flow_key = entry.key().clone();
                                    let capture_type = self.clone();
                                    let (packet_tx, packet_rx) = mpsc::channel(100);
                                    let timeout_tx = timeout_tx.clone();

                                    tokio::spawn(async move {
                                        flow_handler(flow_key, capture_type, packet_rx, timeout_tx).await;
                                    });

                                    entry.insert(packet_tx).send(packet).await?;
                                },
                            }
                        },
                        None => break,
                    }
                },
                Some(flow_key) = timeout_rx.recv() => {
                    // Remove flow. Drops sender and causes its flow_handler to exit.
                    flows.remove(&flow_key);
                },
            }
        }

        tshark.wait().await?;

        Ok(())
    }
}

async fn flow_handler(
    flow_key: FlowKey,
    capture_type: CaptureType,
    mut rx: mpsc::Receiver<Packet>,
    timeout_tx: mpsc::Sender<FlowKey>,
) {
    let opts = match &capture_type {
        CaptureType::Ip { opts, .. } | CaptureType::Wlan { opts, .. } => opts,
    };

    let burst_timeout = Duration::from_secs_f64(1.0 + opts.burst_timeout);
    let flow_timeout = Duration::from_secs_f64(FLOW_TIMEOUT);

    let mut flow = create_flow(&capture_type);

    loop {
        let burst = flow.get_current_burst();

        let timeout = if burst.is_some() {
            sleep(burst_timeout)
        } else {
            sleep(flow_timeout)
        };

        tokio::select! {
            _ = timeout => {
                if let Some(burst) = burst {
                    opts.output_tx.send(burst.clone()).await.unwrap();
                    flow.reset_burst();
                    continue;
                }

                // Flow has timed out due to inactivity. Handler will exit
                // when sender is dropped and None is received.
                timeout_tx.send(flow_key.clone()).await.unwrap();
            },
            packet = rx.recv() => {
                match packet {
                    Some(packet) => {
                        if let Some(burst) = burst {
                            // If packet timestamps do not correlate with program time,
                            // e.g. due to file read, check if burst is ready.
                            if packet.time - burst.end > opts.burst_timeout {
                                opts.output_tx.send(burst.clone()).await.unwrap();
                                flow.reset_burst();
                            }
                        }

                        flow.add_packet(&packet);
                    },
                    None => break,
                }
            },
        }
    }
}

#[derive(Clone, Debug)]
struct Packet {
    time: f64,
    src: String,
    dst: String,
    data_len: u32,
    src_port: u16,
    dst_port: u16,
    seq_number: Option<u16>,
}

impl Packet {
    fn from_tshark(line: &str, capture_type: &CaptureType) -> Result<Self, Box<dyn Error>> {
        let mut fields = line.split_whitespace();

        let time = fields.next().ok_or("no time")?.parse::<f64>()?;
        let src = fields.next().ok_or("no source")?;
        let dst = fields.next().ok_or("no destination")?;
        let data_len = fields.next().ok_or("no length")?.parse::<u32>()?;

        let (mut src_port, mut dst_port, mut seq_number) = (0, 0, None);

        match capture_type {
            CaptureType::Ip {
                aggregate_ports, ..
            } if !aggregate_ports => {
                src_port = fields.next().ok_or("no source port")?.parse::<u16>()?;
                dst_port = fields.next().ok_or("no destination port")?.parse::<u16>()?;
            }
            CaptureType::Wlan { .. } => {
                seq_number = Some(fields.next().ok_or("no sequence number")?.parse::<u16>()?);
            }
            _ => (),
        }

        Ok(Packet {
            time,
            src: src.to_string(),
            dst: dst.to_string(),
            data_len,
            src_port,
            dst_port,
            seq_number,
        })
    }
}

impl Burst {
    fn from_packet(p: &Packet) -> Self {
        Burst {
            src: p.src.clone(),
            dst: p.dst.clone(),
            src_port: p.src_port,
            dst_port: p.dst_port,
            start: p.time,
            end: p.time,
            num_packets: 1,
            size: p.data_len,
        }
    }
}

trait Flow: Send {
    fn add_packet(&mut self, p: &Packet);
    fn get_current_burst(&self) -> &Option<Burst>;
    fn reset_burst(&mut self);
}

fn create_flow(capture_type: &CaptureType) -> Box<dyn Flow> {
    match capture_type {
        CaptureType::Ip { .. } => Box::new(IpFlow {
            current_burst: None,
        }),
        CaptureType::Wlan {
            no_estimation,
            max_deviation,
            .. 
        } => Box::new(WlanFlow {
            no_estimation: *no_estimation,
            max_deviation: *max_deviation,
            expected_seq_number: 0,
            last_packet_len: 0,
            current_burst: None,
        }),
    }
}

struct IpFlow {
    current_burst: Option<Burst>,
}

impl Flow for IpFlow {
    fn add_packet(&mut self, p: &Packet) {
        if self.current_burst.is_none() {
            self.current_burst = Some(Burst::from_packet(p));
            return;
        }

        let burst = self.current_burst.as_mut().unwrap();

        burst.end = p.time;
        burst.num_packets += 1;
        burst.size += p.data_len;
    }

    fn get_current_burst(&self) -> &Option<Burst> {
        &self.current_burst
    }

    fn reset_burst(&mut self) {
        self.current_burst = None;
    }
}

struct WlanFlow {
    no_estimation: bool,
    max_deviation: u16,
    expected_seq_number: u16,
    last_packet_len: u32,
    current_burst: Option<Burst>,
}

impl Flow for WlanFlow {
    fn add_packet(&mut self, p: &Packet) {
        if self.current_burst.is_none() {
            self.current_burst = Some(Burst::from_packet(p));
            self.expected_seq_number = (p.seq_number.unwrap() + 1) & 4095;
            self.last_packet_len = p.data_len;
            return;
        }

        let burst = self.current_burst.as_mut().unwrap();
        let seq_number = p.seq_number.unwrap();

        if seq_number == self.expected_seq_number {
            self.expected_seq_number = (seq_number + 1) & 4095;
            self.last_packet_len = p.data_len;
            burst.end = p.time;
            burst.num_packets += 1;
            burst.size += p.data_len;
            return;
        }

        // Sequence number not what we expect.
        let diff = (seq_number as i16 - self.expected_seq_number as i16) & 4095;
        let signed_diff = if diff <= 2048 { diff } else { diff - 4096 };

        // Check if frame already added. Could be a retransmission.
        // Not enough to filter on the retransmission bit as the first frame might be lost.
        if -(self.max_deviation as i16) < signed_diff && signed_diff < 0 {
            burst.end = p.time;
            return;
        }

        // Sequence number is further along than what we expect. Could be lost frame(s).
        if 0 < signed_diff && signed_diff < self.max_deviation as i16 {
            if !self.no_estimation {
                let estimate = (self.last_packet_len + p.data_len) / 2;
                burst.num_packets += diff as u16;
                burst.size += estimate * diff as u32;
            } else {
                // Accept only this frame if estimation is disabled.
                burst.num_packets += 1;
                burst.size += p.data_len;
            }
            // Bring the expected sequence number in line with the new frame.
            self.expected_seq_number = (seq_number + 1) & 4095;
            self.last_packet_len = p.data_len;
            burst.end = p.time;
        } else {
            // Larger deviation than allowed, go to next expected.
            self.expected_seq_number = (self.expected_seq_number + 1) & 4095;
        }
    }

    fn get_current_burst(&self) -> &Option<Burst> {
        &self.current_burst
    }

    fn reset_burst(&mut self) {
        self.current_burst = None;
    }
}
