// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { ModalContent, ModalDialog, ModalRoot, Stack } from '@nvidia/foundations-react-core'
import { ModelHubContent } from './ModelHubContent'
import { DialogHeader } from '@/ui/components/DialogHeader'
import { EngineType } from '@/shared/types/engines'
import { ModelEntry } from '@/ui/types/model-hub'
import type { ModelItem } from '@/ui/types/engine-info'

export const ModelHubModal = ({
    engine,
    onOpenChange,
    onSubmit,
    downloadedModels
}: {
    engine: EngineType | null
    onOpenChange: (open: boolean) => void
    onSubmit: (models: ModelEntry[]) => void
    /** Already-installed models for this engine. Results matching these are hidden. */
    downloadedModels?: readonly ModelItem[]
}) => {
    return (
        <ModalRoot open={!!engine} onOpenChange={onOpenChange} hideCloseButton>
            <ModalDialog>
                <ModalContent
                    className="no-drag-elements model-hub-modal"
                    style={{
                        width: 'min(720px, calc(100vw - 96px))',
                        maxWidth: 'calc(100vw - 32px)',
                        maxHeight: 'calc(100vh - 32px)'
                    }}
                >
                    <DialogHeader
                        className="no-drag-elements"
                        onClose={() => onOpenChange(false)}
                        divider={false}
                    >
                        Model search
                    </DialogHeader>
                    <Stack
                        gap="4"
                        className="min-h-0 pt-2"
                        style={{ height: 'min(520px, calc(100vh - 180px))' }}
                    >
                        <ModelHubContent
                            engine={engine}
                            multiple={false}
                            onSubmit={onSubmit}
                            downloadedModels={downloadedModels}
                        />
                    </Stack>
                </ModalContent>
            </ModalDialog>
        </ModalRoot>
    )
}
