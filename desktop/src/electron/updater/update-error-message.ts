// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export const UPDATE_CHECK_ERROR_MESSAGE = 'Error checking for updates'
const UPDATE_DOWNLOAD_ERROR_MESSAGE = 'Error downloading update'

export type UpdateOperation = 'check' | 'download'

export function userFacingUpdateError(operation: UpdateOperation): string {
    return operation === 'download' ? UPDATE_DOWNLOAD_ERROR_MESSAGE : UPDATE_CHECK_ERROR_MESSAGE
}
