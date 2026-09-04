// Copyright 2026. SPDX-License-Identifier: Apache-2.0

//! In-place snapshot rollback: rewinds a paused, running microVM to a
//! previously taken snapshot without replacing the Firecracker process.
//!
//! The point is the cost function. Loading a snapshot into a fresh process
//! pays for process/KVM/vCPU/GIC/device construction and then faults the
//! whole working set back in — a price proportional to VM size. Rolling back
//! in place keeps every host resource (fds, eventfds, irqfd/ioeventfd
//! registrations, tap, memory mappings) and only writes back what diverged:
//! dirty pages, vCPU state, and device logical state. The price is
//! proportional to the pages dirtied since the target snapshot.
//!
//! The rollback runs on the VMM thread while the VM is paused, so the world
//! is frozen: no vCPU executes, no device event fires, nothing observes the
//! intermediate state.
//!
//! Failure model: everything before the memory write-back is validation and
//! bookkeeping with no side effects on guest state — an error leaves the VM
//! paused and resumable. From the memory write-back onwards the VM is a mix
//! of two moments in time until the sequence completes, so any error there
//! marks the VM `Faulted`: resume and snapshot operations are refused and the
//! orchestrator is expected to replace the process (its kill-and-rebuild
//! fallback path).

use std::fs::File;
use std::io::Read;
use std::os::unix::io::AsRawFd;
use std::time::Instant;

use crate::arch::DeviceType;
use crate::devices::virtio::persist::MmioTransportState;
use crate::devices::virtio::vsock::TYPE_VSOCK;
use crate::devices::virtio::{TYPE_BALLOON, TYPE_BLOCK, TYPE_NET};
use crate::logger::info;
use crate::persist::snapshot_state_from_file;
use crate::utils::{get_page_size, u64_to_usize};
use crate::vmm_config::instance_info::VmState;
use crate::vmm_config::snapshot::{
    RollbackResponse, RollbackSnapshotParams, RollbackTimings,
};
use crate::vstate::memory::{
    Bitmap, GuestMemory, GuestMemoryExtension, GuestMemoryMmap, GuestMemoryRegion,
    deserialize_dirty_bitmap, serialize_dirty_bitmap,
};
use crate::{DirtyBitmap, MicrovmState, Vmm};

/// Errors of an in-place rollback. Split by whether the VM survives them.
#[derive(Debug, thiserror::Error, displaydoc::Display)]
pub enum RollbackError {
    /// Rollback requires the microVM to be paused
    NotPaused,
    /// The microVM is faulted from an earlier failed rollback
    Faulted,
    /// Cannot load the snapshot file: {0}
    SnapshotFile(String),
    /// Snapshot does not match the running microVM: {0}
    Validation(String),
    /// Cannot read the revert bitmap: {0}
    RevertBitmap(String),
    /// Cannot open the memory file: {0}
    MemoryFile(String),
    /// Cannot read the live dirty bitmap: {0}
    DirtyBitmap(String),
    /// Cannot write the dirty bitmap file: {0}
    BitmapWrite(String),
    /// Reverting guest memory failed: {0}
    Memory(String),
    /// Restoring vCPU state failed: {0}
    Vcpu(String),
    /// Restoring interrupt controller state failed: {0}
    Gic(String),
    /// Restoring device state failed: {0}
    Devices(String),
}

impl RollbackError {
    /// Whether the failure happened past the commit point, leaving guest
    /// state torn between two moments in time.
    pub fn faults_vm(&self) -> bool {
        match self {
            RollbackError::NotPaused
            | RollbackError::Faulted
            | RollbackError::SnapshotFile(_)
            | RollbackError::Validation(_)
            | RollbackError::RevertBitmap(_)
            | RollbackError::MemoryFile(_)
            | RollbackError::DirtyBitmap(_)
            | RollbackError::BitmapWrite(_) => false,
            RollbackError::Memory(_)
            | RollbackError::Vcpu(_)
            | RollbackError::Gic(_)
            | RollbackError::Devices(_) => true,
        }
    }
}

fn micros(from: Instant) -> u64 {
    u64::try_from(from.elapsed().as_micros()).unwrap_or(u64::MAX)
}

/// Rolls the paused microVM back to the given snapshot in place.
///
/// On success the VM is left paused (the caller resumes it if asked to); the
/// dirty-page baseline is reset so the next Diff snapshot is relative to
/// exactly the restored state.
pub fn rollback_snapshot(
    vmm: &mut Vmm,
    params: &RollbackSnapshotParams,
) -> Result<RollbackResponse, RollbackError> {
    let start = Instant::now();
    let mut timings = RollbackTimings::default();

    // ── Phase 1: validation. No side effects on guest state. ──
    match vmm.instance_info.state {
        VmState::Paused => {}
        VmState::Faulted => return Err(RollbackError::Faulted),
        _ => return Err(RollbackError::NotPaused),
    }

    let state = snapshot_state_from_file(&params.snapshot_path)
        .map_err(|err| RollbackError::SnapshotFile(err.to_string()))?;

    let page_size = get_page_size()
        .map_err(|err| RollbackError::Validation(format!("page size: {err}")))?;
    let mem_size: u64 = vmm.vm.guest_memory().iter().map(|r| r.len()).sum();
    let total_pages = u64_to_usize(mem_size) / page_size;

    validate_topology(vmm, &state, mem_size)?;

    let mut mem_file = File::open(&params.mem_file_path)
        .map_err(|err| RollbackError::MemoryFile(err.to_string()))?;
    let mem_file_len = mem_file
        .metadata()
        .map_err(|err| RollbackError::MemoryFile(err.to_string()))?
        .len();
    if mem_file_len != mem_size {
        return Err(RollbackError::Validation(format!(
            "memory file is {mem_file_len} bytes, guest memory is {mem_size}"
        )));
    }

    let file_bitmap = match &params.revert_bitmap_path {
        None => None,
        Some(path) => {
            let mut data = Vec::new();
            File::open(path)
                .and_then(|mut f| f.read_to_end(&mut data))
                .map_err(|err| RollbackError::RevertBitmap(err.to_string()))?;
            let (bm_page_size, bm_pages, words) = deserialize_dirty_bitmap(&data)
                .map_err(|err| RollbackError::RevertBitmap(err.to_string()))?;
            if bm_page_size != page_size || bm_pages != total_pages {
                return Err(RollbackError::RevertBitmap(format!(
                    "bitmap describes {bm_pages} pages of {bm_page_size} bytes, guest has \
                     {total_pages} pages of {page_size}"
                )));
            }
            Some(words)
        }
    };
    timings.validate = micros(start);

    // ── Phase 2: quiesce in-flight device I/O. ──
    let phase = Instant::now();
    quiesce_devices(vmm)?;
    timings.quiesce = micros(phase);

    // ── Phase 3: take the live dirty bitmap. KVM's read is destructive, so
    // the result is folded into the userspace bitmap first: whatever happens
    // next, the information "these pages diverged" is never lost. ──
    let phase = Instant::now();
    let kvm_bitmap: DirtyBitmap = vmm
        .vm
        .get_dirty_bitmap()
        .map_err(|err| RollbackError::DirtyBitmap(err.to_string()))?;
    vmm.vm
        .guest_memory()
        .store_dirty_bitmap(&kvm_bitmap, page_size);

    // revert set = live dirty pages ∪ caller-provided cumulative set.
    let mut revert = userspace_bitmap_flat(vmm.vm.guest_memory(), page_size, total_pages);
    if let Some(words) = &file_bitmap {
        for (dst, src) in revert.iter_mut().zip(words.iter()) {
            *dst |= *src;
        }
    }

    // ────────── commit point ──────────
    // Guest memory changes from here on. Any failure now leaves a VM that is
    // partly at the target snapshot and partly at the present: Faulted.

    // ── Phase 4: revert memory. ──
    let (restored_pages, restored_bytes) = vmm
        .vm
        .guest_memory()
        .restore_dirty(&mut mem_file, &revert, page_size)
        .map_err(|err| RollbackError::Memory(err.to_string()))?;
    timings.memory = micros(phase);

    // ── Phase 5: vCPUs, in their own threads like every state operation. ──
    let phase = Instant::now();
    let mut state = state;
    let vcpu_states = std::mem::take(&mut state.vcpu_states);
    #[cfg(target_arch = "aarch64")]
    let mpidrs = crate::construct_kvm_mpidrs(&vcpu_states);
    // vCPU route is fixed to reinit (the KVM-defined reset + full restore):
    // it won the G1 gate (p95 48.3ms vs 145.3ms) and the registers-only
    // route was removed with the measurement.
    vmm.restore_vcpu_states_in_place(vcpu_states)
        .map_err(|err| RollbackError::Vcpu(format!("{err:?}")))?;
    timings.vcpus = micros(phase);

    // ── Phase 6: interrupt controller, against the existing device fd. ──
    let phase = Instant::now();
    #[cfg(target_arch = "aarch64")]
    vmm.vm
        .restore_state(&mpidrs, &state.vm_state)
        .map_err(|err| RollbackError::Gic(err.to_string()))?;
    #[cfg(target_arch = "x86_64")]
    vmm.vm
        .restore_state(&state.vm_state)
        .map_err(|err| RollbackError::Gic(err.to_string()))?;
    timings.gic = micros(phase);

    // ── Phase 7: device logical state, written onto the live objects. ──
    let phase = Instant::now();
    apply_device_states(vmm, &state)?;

    // Serial console internal state is not part of snapshots; re-init it the
    // same way the ordinary restore path does.
    vmm.emulate_serial_init()
        .map_err(|err| RollbackError::Devices(format!("serial init: {err}")))?;

    // ── Phase 8: VMGenID. The guest must learn that time was rewound; a new
    // generation (never the snapshot's) is written after the memory revert so
    // the revert cannot clobber it. ──
    if let Some(vmgenid) = vmm.acpi_device_manager.vmgenid.as_mut() {
        vmgenid
            .refresh_generation(vmm.vm.guest_memory())
            .map_err(|err| RollbackError::Devices(format!("vmgenid: {err}")))?;
    } else {
        info!("rollback: no VMGenID device; guest is not notified of the rewind");
    }
    timings.devices = micros(phase);

    // ── Phase 9: reset the dirty-page baseline. The next Diff snapshot must
    // be relative to exactly this restored state. Our own restore writes went
    // through the userspace bitmap, so both sides are cleared, and queue
    // pages are re-marked because runtime queue writes are not tracked. ──
    vmm.vm.reset_dirty_bitmap();
    vmm.vm.guest_memory().reset_dirty();
    vmm.mmio_device_manager
        .for_each_virtio_device(|_, _, _, dev| {
            let d = dev.lock().expect("Poisoned lock");
            if d.is_activated() {
                d.mark_queue_memory_dirty(vmm.vm.guest_memory())
            } else {
                Ok(())
            }
        })
        .map_err(|err: crate::devices::virtio::queue::QueueError| {
            RollbackError::Devices(format!("marking queue memory dirty: {err}"))
        })?;

    // Phase 10, kicking the devices to reprocess their (rewound) queues,
    // happens inside resume_vm — it kicks unconditionally on every resume.

    timings.total = micros(start);

    Ok(RollbackResponse {
        restored_pages,
        restored_bytes,
        timings_us: timings,
    })
}

/// Checks that the snapshot describes the running microVM: same vCPUs, same
/// memory size, same device set. Rollback writes state onto live objects — a
/// topology difference means those objects do not exist or differ, which a
/// rebuild-from-snapshot handles and an in-place write cannot.
fn validate_topology(
    vmm: &Vmm,
    state: &MicrovmState,
    mem_size: u64,
) -> Result<(), RollbackError> {
    let fail = |msg: String| Err(RollbackError::Validation(msg));

    if state.vcpu_states.len() != vmm.vcpus_handles.len() {
        return fail(format!(
            "snapshot has {} vcpus, microVM has {}",
            state.vcpu_states.len(),
            vmm.vcpus_handles.len()
        ));
    }

    let snapshot_mem = state.vm_info.mem_size_mib * 1024 * 1024;
    if snapshot_mem != mem_size {
        return fail(format!(
            "snapshot guest memory is {snapshot_mem} bytes, microVM has {mem_size}"
        ));
    }

    if state.device_states.balloon_device.is_some() {
        return fail("snapshot contains a balloon device".to_string());
    }
    if state.device_states.vsock_device.is_some() {
        return fail("snapshot contains a vsock device".to_string());
    }

    // Collect the live virtio device set.
    let mut live: Vec<(u32, String, bool)> = Vec::new();
    vmm.mmio_device_manager
        .for_each_virtio_device(|ty, id, _, dev| {
            let activated = dev.lock().expect("Poisoned lock").is_activated();
            live.push((ty, id.clone(), activated));
            Ok::<(), std::convert::Infallible>(())
        })
        .unwrap();

    if live
        .iter()
        .any(|(ty, _, _)| *ty == TYPE_BALLOON || *ty == TYPE_VSOCK)
    {
        return fail("microVM has a balloon or vsock device".to_string());
    }

    let expect_device = |ty: u32, id: &str, activated: bool| -> Result<(), RollbackError> {
        match live.iter().find(|(t, i, _)| *t == ty && i == id) {
            None => Err(RollbackError::Validation(format!(
                "snapshot device {id} (type {ty}) does not exist in the microVM"
            ))),
            Some((_, _, live_activated)) if *live_activated != activated => {
                Err(RollbackError::Validation(format!(
                    "device {id}: snapshot activated={activated}, live activated={live_activated}"
                )))
            }
            Some(_) => Ok(()),
        }
    };

    let mut snapshot_count = 0;
    for block in &state.device_states.block_devices {
        snapshot_count += 1;
        let virtio_state = block.device_state.virtio_state().ok_or_else(|| {
            RollbackError::Validation(format!(
                "device {}: vhost-user block cannot be rolled back in place",
                block.device_id
            ))
        })?;
        expect_device(TYPE_BLOCK, &block.device_id, virtio_state.activated)?;
    }
    for net in &state.device_states.net_devices {
        snapshot_count += 1;
        expect_device(TYPE_NET, &net.device_id, net.device_state.virtio_state().activated)?;
    }
    if let Some(entropy) = &state.device_states.entropy_device {
        snapshot_count += 1;
        expect_device(
            crate::devices::virtio::TYPE_RNG,
            &entropy.device_id,
            entropy.device_state.virtio_state().activated,
        )?;
    }

    if snapshot_count != live.len() {
        return fail(format!(
            "snapshot describes {snapshot_count} virtio devices, microVM has {}",
            live.len()
        ));
    }

    Ok(())
}

/// Drains in-flight device I/O so no completion lands after the state has
/// been rewound: block waits out its async engine, net reads and discards
/// whatever the tap buffered — those frames arrived for the timeline being
/// discarded.
fn quiesce_devices(vmm: &mut Vmm) -> Result<(), RollbackError> {
    vmm.mmio_device_manager
        .for_each_virtio_device(|ty, _, _, dev| {
            let mut d = dev.lock().expect("Poisoned lock");
            match ty {
                TYPE_BLOCK => {
                    d.as_mut_any()
                        .downcast_mut::<crate::devices::virtio::block::device::Block>()
                        .expect("block device downcast")
                        .prepare_save();
                }
                TYPE_NET => {
                    let net = d
                        .as_mut_any()
                        .downcast_mut::<crate::devices::virtio::net::Net>()
                        .expect("net device downcast");
                    // Frames the tap buffered were sent to the timeline being
                    // discarded; read them out so they cannot surface after
                    // the rewind. The tap is non-blocking — stop on error
                    // (EAGAIN) or a sane cap.
                    let fd = net.tap.as_raw_fd();
                    let mut buf = [0u8; 65562];
                    for _ in 0..4096 {
                        // SAFETY: fd is a valid tap fd and buf is a valid
                        // writable buffer of the stated length.
                        let n = unsafe {
                            libc::read(fd, buf.as_mut_ptr().cast(), buf.len())
                        };
                        if n <= 0 {
                            break;
                        }
                    }
                }
                _ => {}
            }
            Ok::<(), std::convert::Infallible>(())
        })
        .unwrap();

    Ok(())
}

/// Writes the current live dirty bitmap (pages dirtied since the last
/// snapshot or rollback) to `path` in the FCDB sidecar format.
///
/// Requires the microVM to be paused. KVM's read is destructive, so the
/// result is folded into the userspace bitmap first — the same invariant
/// rollback's Phase 3 maintains: whatever happens next, the next Diff
/// snapshot or rollback still sees the full set.
pub fn save_dirty_bitmap(vmm: &mut Vmm, path: &std::path::Path) -> Result<(), RollbackError> {
    match vmm.instance_info.state {
        VmState::Paused => {}
        VmState::Faulted => return Err(RollbackError::Faulted),
        _ => return Err(RollbackError::NotPaused),
    }

    let page_size = get_page_size()
        .map_err(|err| RollbackError::Validation(format!("page size: {err}")))?;
    let mem_size: u64 = vmm.vm.guest_memory().iter().map(|r| r.len()).sum();
    let total_pages = u64_to_usize(mem_size) / page_size;

    let kvm_bitmap: DirtyBitmap = vmm
        .vm
        .get_dirty_bitmap()
        .map_err(|err| RollbackError::DirtyBitmap(err.to_string()))?;
    vmm.vm
        .guest_memory()
        .store_dirty_bitmap(&kvm_bitmap, page_size);

    let words = userspace_bitmap_flat(vmm.vm.guest_memory(), page_size, total_pages);
    let data = serialize_dirty_bitmap(&words, page_size, total_pages);
    std::fs::write(path, data).map_err(|err| RollbackError::BitmapWrite(err.to_string()))?;

    Ok(())
}

/// Builds a flat page bitmap (file-offset indexed, regions tiled) from the
/// userspace dirty bitmaps of all regions.
fn userspace_bitmap_flat(
    guest_memory: &GuestMemoryMmap,
    page_size: usize,
    total_pages: usize,
) -> Vec<u64> {
    let mut flat = vec![0u64; total_pages.div_ceil(64)];
    let mut region_start_page = 0usize;

    for region in guest_memory.iter() {
        let region_pages = u64_to_usize(region.len()) / page_size;
        if let Some(bitmap) = region.bitmap() {
            for page in 0..region_pages {
                if bitmap.dirty_at(page * page_size) {
                    let flat_page = region_start_page + page;
                    flat[flat_page / 64] |= 1u64 << (flat_page % 64);
                }
            }
        }
        region_start_page += region_pages;
    }

    flat
}

/// Writes the snapshot's device state onto the live devices: virtio queues,
/// negotiated features and interrupt status through the `VirtioDevice` trait,
/// transport registers onto the live `MmioTransport`. Every host resource —
/// eventfds, irqfd/ioeventfd registrations, the tap, disk fds — stays.
fn apply_device_states(vmm: &mut Vmm, state: &MicrovmState) -> Result<(), RollbackError> {
    for block in &state.device_states.block_devices {
        let virtio_state = block.device_state.virtio_state().ok_or_else(|| {
            RollbackError::Devices(format!(
                "device {}: vhost-user block cannot be rolled back in place",
                block.device_id
            ))
        })?;
        apply_one_device(vmm, TYPE_BLOCK, &block.device_id, virtio_state, &block.transport_state)?;
    }
    for net in &state.device_states.net_devices {
        apply_one_device(
            vmm,
            TYPE_NET,
            &net.device_id,
            net.device_state.virtio_state(),
            &net.transport_state,
        )?;
        apply_net_rx_cache(
            vmm,
            &net.device_id,
            net.device_state.rx_buffers_state().parsed_descriptor_chains_nr,
            net.device_state.rx_buffers_state().used_descriptors,
            net.device_state.rx_buffers_state().used_bytes,
        )?;
    }
    if let Some(entropy) = &state.device_states.entropy_device {
        apply_one_device(
            vmm,
            crate::devices::virtio::TYPE_RNG,
            &entropy.device_id,
            entropy.device_state.virtio_state(),
            &entropy.transport_state,
        )?;
    }

    log_queue_diagnostics(vmm);

    Ok(())
}

/// Logs every activated virtio device's queue positions after the apply.
///
/// The rare post-rollback hang (guest idle, RX dead) leaves no evidence once
/// the sandbox is torn down; one line per device here lets a reproduction be
/// diagnosed from the log alone. avail/used are read from (reverted) guest
/// memory exactly the way the device reads them.
pub(crate) fn log_queue_diagnostics(vmm: &Vmm) {
    vmm.mmio_device_manager
        .for_each_virtio_device(|ty, id, _, dev| {
            let mut d = dev.lock().expect("Poisoned lock");
            if !d.is_activated() {
                return Ok::<(), std::convert::Infallible>(());
            }
            let mut parts: Vec<String> = Vec::new();
            for (i, q) in d.queues().iter().enumerate() {
                parts.push(format!(
                    "q{i} avail={} next_avail={} next_used={} pending={}",
                    q.avail_ring_idx_get(),
                    q.next_avail.0,
                    q.next_used.0,
                    q.len(),
                ));
            }
            if ty == TYPE_NET {
                if let Some(net) = d
                    .as_mut_any()
                    .downcast_mut::<crate::devices::virtio::net::Net>()
                {
                    parts.push(format!(
                        "rx_buffer parsed={} used_desc={} used_bytes={}",
                        net.rx_buffer.parsed_descriptors.len(),
                        net.rx_buffer.used_descriptors,
                        net.rx_buffer.used_bytes,
                    ));
                }
            }
            info!("rollback: device {id} (type {ty}) {}", parts.join(" | "));
            Ok::<(), std::convert::Infallible>(())
        })
        .unwrap();
}

/// Rebuilds the net device's parsed-RX-descriptor cache after the queues were
/// rewound.
///
/// apply_one_device rewinds the queue indices, but the live device still holds
/// descriptor chains it parsed out of the avail ring on the timeline being
/// discarded. Those ring pages have just been reverted, so the cached heads no
/// longer describe buffers the guest believes it posted: the first frame
/// completed after the rewind writes a used entry carrying a stale descriptor
/// id, the guest driver rejects it ("id N is not a head!"), and RX wedges for
/// good. The process-replacing restore never hits this because it builds the
/// device from scratch; in place the same steps have to be redone on the live
/// object.
fn apply_net_rx_cache(
    vmm: &Vmm,
    id: &str,
    parsed_descriptor_chains_nr: u16,
    used_descriptors: u16,
    used_bytes: u32,
) -> Result<(), RollbackError> {
    let bus_device = vmm
        .mmio_device_manager
        .get_device(DeviceType::Virtio(TYPE_NET), id)
        .ok_or_else(|| RollbackError::Devices(format!("device {id} disappeared")))?;
    let mut bus = bus_device.lock().expect("Poisoned lock");
    let transport = bus
        .mmio_transport_mut()
        .ok_or_else(|| RollbackError::Devices(format!("device {id} is not an mmio transport")))?;
    let device = transport.device();
    let mut device = device.lock().expect("Poisoned lock");
    device
        .as_mut_any()
        .downcast_mut::<crate::devices::virtio::net::Net>()
        .expect("net device downcast")
        .rollback_rx_buffers(parsed_descriptor_chains_nr, used_descriptors, used_bytes);

    Ok(())
}

fn apply_one_device(
    vmm: &Vmm,
    ty: u32,
    id: &str,
    virtio_state: &crate::devices::virtio::persist::VirtioDeviceState,
    transport_state: &MmioTransportState,
) -> Result<(), RollbackError> {
    let bus_device = vmm
        .mmio_device_manager
        .get_device(DeviceType::Virtio(ty), id)
        .ok_or_else(|| RollbackError::Devices(format!("device {id} disappeared")))?;

    let mut bus = bus_device.lock().expect("Poisoned lock");
    let transport = bus
        .mmio_transport_mut()
        .ok_or_else(|| RollbackError::Devices(format!("device {id} is not an mmio transport")))?;

    // Transport registers first — plain field writes, no side effects.
    transport.apply_state(transport_state);

    let device = transport.device();
    let mut device = device.lock().expect("Poisoned lock");

    // Rebuild the queues from the snapshot against current guest memory (the
    // memory was reverted before this, so ring contents and indices are of
    // the same moment) and copy them onto the live queue objects.
    let expected_queues = device.queues().len();
    let expected_max_size = device
        .queues()
        .first()
        .map(|q| q.max_size)
        .unwrap_or_default();
    let queues = virtio_state
        .build_queues_checked(
            vmm.vm.guest_memory(),
            ty,
            expected_queues,
            expected_max_size,
        )
        .map_err(|err| RollbackError::Devices(format!("device {id} queues: {err}")))?;
    for (live, snapshot) in device.queues_mut().iter_mut().zip(queues) {
        *live = snapshot;
    }

    device.set_acked_features(virtio_state.acked_features);
    device
        .interrupt_status()
        .store(virtio_state.interrupt_status, std::sync::atomic::Ordering::SeqCst);

    Ok(())
}
