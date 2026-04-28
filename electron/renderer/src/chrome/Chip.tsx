import type { ReactNode } from 'react'

export type ChipVariant = 'approved' | 'revise' | 'fix' | 'pending'

interface ChipProps {
  variant: ChipVariant
  children: ReactNode
}

// The verdict chip used under the VRAM slot and on review cards.
export default function Chip({ variant, children }: ChipProps) {
  return <span className={`chip ${variant}`}>{children}</span>
}
