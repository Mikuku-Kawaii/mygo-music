; Cover installs keep the app directory. Remove the retired bundled resources.
!macro customInstall
  Delete "$INSTDIR\media-downloads\index.js"
  Delete "$INSTDIR\media-downloads\plugin.json"
  Delete "$INSTDIR\media-downloads\locales\zh-CN.json"
  RMDir "$INSTDIR\media-downloads\locales"
  RMDir "$INSTDIR\media-downloads"
  Delete "$INSTDIR\download-tools\yt-dlp.exe"
  Delete "$INSTDIR\download-tools\yt-dlp-LICENSE.txt"
  Delete "$INSTDIR\download-tools\yt-dlp-SOURCE.txt"
  Delete "$INSTDIR\download-tools\yt-dlp-THIRD_PARTY_LICENSES.txt"
  RMDir "$INSTDIR\download-tools"
!macroend
