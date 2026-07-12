function check(a,b){if(a!==b)throw a+" !== "+b;} check(1===1,true);check(1==='1',false);check(NaN===NaN,false);check(null===null,true);check(undefined===undefined,true);console.log('PASS');
