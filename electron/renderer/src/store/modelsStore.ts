import { create } from 'zustand'
import type { ModelInfo } from '../lib/daemonClient'

// Cached /models response. Populated once when the main UI mounts
// because the daemon treats this as a relatively static catalog.
// ModelDropdown reads directly from this store.
export interface ModelsState {
  list: ModelInfo[]
  setList: (list: ModelInfo[]) => void
}

export const useModelsStore = create<ModelsState>((set) => ({
  list: [],
  setList: (list) => set({ list }),
}))
