// Copyright 2023 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

use serde::{Deserialize, Serialize};

use super::vhost_user::persist::VhostUserBlockState;
use super::virtio::persist::VirtioBlockState;
use crate::vstate::memory::GuestMemoryMmap;

/// Block device state.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum BlockState {
    Virtio(VirtioBlockState),
    VhostUser(VhostUserBlockState),
}

impl BlockState {
    /// The generic virtio half of this state, used by in-place rollback.
    /// vhost-user block is not supported there — its state lives in the
    /// backend process, which an in-place write cannot reach.
    pub(crate) fn virtio_state(&self) -> Option<&crate::devices::virtio::persist::VirtioDeviceState> {
        match self {
            BlockState::Virtio(state) => Some(state.virtio_state()),
            BlockState::VhostUser(_) => None,
        }
    }
}

/// Auxiliary structure for creating a device when resuming from a snapshot.
#[derive(Debug)]
pub struct BlockConstructorArgs {
    pub mem: GuestMemoryMmap,
}
