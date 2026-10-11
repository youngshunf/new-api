package constant

import "github.com/QuantumNous/new-api/relaykit/types"

// EndpointType moved to types with the conversion kit; aliases keep host
// code compiling unchanged.
type EndpointType = types.EndpointType

const (
	EndpointTypeOpenAI                             = types.EndpointTypeOpenAI
	EndpointTypeOpenAIResponse                     = types.EndpointTypeOpenAIResponse
	EndpointTypeOpenAIResponseCompact              = types.EndpointTypeOpenAIResponseCompact
	EndpointTypeOpenAIAlphaSearch                  = types.EndpointTypeOpenAIAlphaSearch
	EndpointTypeAnthropic                          = types.EndpointTypeAnthropic
	EndpointTypeGemini                             = types.EndpointTypeGemini
	EndpointTypeJinaRerank                         = types.EndpointTypeJinaRerank
	EndpointTypeImageGeneration                    = types.EndpointTypeImageGeneration
	EndpointTypeEmbeddings                         = types.EndpointTypeEmbeddings
	EndpointTypeOpenAIVideo                        = types.EndpointTypeOpenAIVideo
	EndpointTypeOpenAIAudioSpeech     EndpointType = "openai_audio_speech"
	EndpointTypeHasnAudioVoice        EndpointType = "hasn_audio_voice"
)
