import { noop } from './utils';
import { HandlerFunction, WorkerInterface } from './worker_interface';

const defaultRequestOptions = {
	method: 'POST',
	headers: {
		'Content-Type': 'application/x-protobuf',
	},
};

export const setupHttpWorker = (baseURL: string) => {
	const makeHttpApiRequest = (endPoint: string, inputData: Uint8Array, requestId: string) =>
		fetch(`${baseURL}/${endPoint}?requestId=${requestId}`, {
			...defaultRequestOptions,
			body: inputData,
		});

	const syncHandler: HandlerFunction = async (inputData, _, id, msg) => {
		const response = await makeHttpApiRequest(msg, inputData, id);
		const ab = await response.arrayBuffer();
		return new Uint8Array(ab);
	};

	const asyncHandler: HandlerFunction = async (inputData, progress, id, msg) => {
		const asyncApiResult = await syncHandler(inputData, noop, id, msg);
		let outputData = new Uint8Array();
		while (true) {
			const progressResponse = await makeHttpApiRequest('asyncProgress', asyncApiResult, id);

			// If no new data available, stop querying.
			if ([204, 404].includes(progressResponse.status)) {
				break;
			}

			const ab = await progressResponse.arrayBuffer();
			outputData = new Uint8Array(ab);
			progress(outputData);
			// No client-side wait: the server long-polls /asyncProgress, holding each request open
			// until there is new progress or completion. The next request paces itself naturally.
		}
		return outputData;
	};

	const noWasmConcurrency: HandlerFunction = (_, __, msg) => {
		const errmsg = `Tried to use ${msg} while using a http worker! This is only supported for wasm!`;
		console.error(errmsg);
		return new Uint8Array();
	};

	new WorkerInterface({
		computeStats: syncHandler,
		computeStatsJson: syncHandler,
		raidSim: syncHandler,
		raidSimJson: syncHandler,
		raidSimAsync: asyncHandler,
		bulkSimAsync: asyncHandler,
		bulkComboSimAsync: asyncHandler,
		statWeights: syncHandler,
		statWeightsAsync: asyncHandler,
		statWeightRequests: syncHandler,
		statWeightCompute: syncHandler,
		raidSimRequestSplit: noWasmConcurrency,
		raidSimResultCombination: noWasmConcurrency,
		abortById: syncHandler,
	}).ready(false);
};
