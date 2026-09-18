import { type ChangeEvent, useRef } from 'react'
import { toast } from 'sonner'
import { api } from '../../../lib/api'
import { logger } from '../../../lib/logger'
import { Button } from '../../ui/button'
import { Download, Upload } from '../../ui/icons'

interface ModelOverridesControlsProps {
  providerName: string
}

// ModelOverridesControls provides the download/upload entry points for a
// provider's manual model-override document. Downloads fetch the stored
// document (or an empty template when none is stored) as a JSON file; uploads
// read a JSON file and replace the stored document. Manual modelling is edited
// through this file only — see the priority model-source chain in the backend.
export function ModelOverridesControls({ providerName }: ModelOverridesControlsProps) {
  const fileInputRef = useRef<HTMLInputElement>(null)

  const handleDownload = async () => {
    try {
      const overrides = await api.providers.getModelOverrides(providerName)
      const blob = new Blob([JSON.stringify(overrides, null, 2)], { type: 'application/json' })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = `${providerName}-models.json`
      document.body.appendChild(a)
      a.click()
      document.body.removeChild(a)
      URL.revokeObjectURL(url)
      toast.success('Models JSON downloaded', {
        description: `${providerName} model overrides saved to a JSON file.`,
      })
    } catch (err) {
      logger.error('Failed to download model overrides:', err)
      toast.error('Download failed', { description: `Could not download ${providerName} model overrides.` })
    }
  }

  const triggerUpload = () => fileInputRef.current?.click()

  const handleFileChange = async (e: ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    e.target.value = '' // allow re-selecting the same file
    if (!file) return

    try {
      const text = await file.text()
      await api.providers.putModelOverrides(providerName, text)
      toast.success('Models JSON uploaded', {
        description: `${providerName} model overrides updated from ${file.name}.`,
      })
    } catch (err) {
      logger.error('Failed to upload model overrides:', err)
      toast.error('Upload failed', {
        description: `Could not upload model overrides for ${providerName}.`,
      })
    }
  }

  return (
    <>
      <input
        ref={fileInputRef}
        type="file"
        accept=".json,application/json"
        className="hidden"
        onChange={handleFileChange}
      />
      <Button variant="ghost" size="sm" onClick={handleDownload} title="Download model overrides JSON">
        <Download size={14} />
        Models JSON
      </Button>
      <Button variant="ghost" size="sm" onClick={triggerUpload} title="Upload model overrides JSON file">
        <Upload size={14} />
        Upload
      </Button>
    </>
  )
}
